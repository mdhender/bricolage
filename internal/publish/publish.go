// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/jobs"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/store"
)

// Options configure a Publisher. Everything is resolved before New is called;
// this package reads no flags, no environment, and no configuration file.
type Options struct {
	// DB, Renderer, and Tree are required: a publisher with no template tree
	// renders nothing and a publisher with no output tree has nowhere to put
	// what it rendered. A server started without either has no Publisher at
	// all, and the service answers 503 naming the flag.
	DB       *store.DB
	Renderer *render.Engine
	Tree     *Tree

	// Clock is required. There is no default, deliberately: a publisher that
	// silently fell back to the wall clock is a publisher whose scheduling
	// tests pass for the wrong reason (invariant 3).
	Clock clock.Clock

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger
}

// Publisher renders documents to the output tree and remembers what it wrote
// (DESIGN.md 8.3, PLAN.md M9).
type Publisher struct {
	db       *store.DB
	renderer *render.Engine
	tree     *Tree
	clock    clock.Clock
	log      *slog.Logger
}

// There is deliberately no *jobs.Queue here.
//
// A publish schedules the expiry of every address it stopped producing, and it
// does that inside the store's transaction rather than through the queue: the
// rows that say what to expire are discovered by the DELETE that removes them,
// and enqueuing through a second connection afterwards would be a second
// transaction that could commit the deletion and lose the expiry. So this
// package builds the job and internal/store writes it, which is also why the
// composition root can build the publisher before the service that owns the
// process's one queue.

// New builds a publisher.
func New(opts Options) (*Publisher, error) {
	switch {
	case opts.DB == nil:
		return nil, fmt.Errorf("publish: no database")
	case opts.Renderer == nil:
		return nil, fmt.Errorf("publish: no template tree")
	case opts.Tree == nil:
		return nil, fmt.Errorf("publish: no output tree")
	case opts.Clock == nil:
		return nil, fmt.Errorf("publish: no clock; every component that needs the time is given one (invariant 3)")
	}
	p := &Publisher{
		db:       opts.DB,
		renderer: opts.Renderer,
		tree:     opts.Tree,
		clock:    opts.Clock,
		log:      opts.Logger,
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p, nil
}

// now reads the injected clock. Nothing in this package calls time.Now
// (invariant 3).
func (p *Publisher) now() time.Time { return p.clock.Now().UTC() }

// Register adds the two job kinds this package handles (PLAN.md M9).
//
// They are registered from here rather than in internal/jobs beside the noop
// kind because the handler needs a publisher, and a registry that had to know
// how to build one would be a registry that imported half the system. The
// composition root builds the publisher and hands it here.
func (p *Publisher) Register(r *jobs.Registry) {
	r.Register(domain.KindPublish, jobs.HandlerFunc(func(ctx context.Context, job domain.Job) error {
		payload, err := domain.ParsePublishPayload(job.Payload)
		if err != nil {
			return err
		}
		result, err := p.Publish(ctx, payload)
		if err != nil {
			return err
		}
		p.log.Info("published",
			"document", result.Document.UID,
			"version", result.Version.Number,
			"resources", len(result.Written),
			"expired", len(result.Expired),
		)
		return nil
	}))

	r.Register(domain.KindExpire, jobs.HandlerFunc(func(ctx context.Context, job domain.Job) error {
		payload, err := domain.ParseExpirePayload(job.Payload)
		if err != nil {
			return err
		}
		return p.Expire(ctx, payload)
	}))
}

// Result is what one publish did.
type Result struct {
	Document domain.Document
	Version  domain.Version

	// Written are the resource rows as they now stand, and Expired are the
	// addresses this publish stopped producing. Jobs are the expire jobs
	// scheduled for them.
	Written []domain.Resource
	Expired []domain.Resource
	Jobs    []domain.Job
}

// Publish renders a pinned version into every named output channel, writes the
// files, records the resources, and expires the addresses it no longer
// produces.
//
// The version is the payload's and never the document's current one
// (invariant 8). That is the single most important line in this package: an
// editor approves version 5 for midnight and keeps working, and at midnight
// version 5 is what appears.
func (p *Publisher) Publish(ctx context.Context, payload domain.PublishPayload) (Result, error) {
	if err := payload.Validate(); err != nil {
		return Result{}, err
	}

	doc, err := p.db.DocumentByID(ctx, payload.DocumentID)
	if err != nil {
		return Result{}, err
	}
	version, err := p.db.VersionByID(ctx, payload.VersionID)
	if err != nil {
		return Result{}, err
	}
	if version.DocumentID != doc.ID {
		return Result{}, fmt.Errorf(
			"version %d belongs to document %d and this publish names document %s: %w",
			version.ID, version.DocumentID, doc.UID, domain.ErrInvalid)
	}
	if version.IsDraft() {
		// A pin on an open working draft is a pin on something that can still
		// change, which is the one thing invariant 8 promises cannot happen.
		return Result{}, fmt.Errorf(
			"document %s: version %d is an open working draft and a publish pins a checked-in version: %w",
			doc.UID, version.Number, domain.ErrConflict)
	}

	site, err := p.db.SiteByID(ctx, doc.SiteID)
	if err != nil {
		return Result{}, err
	}

	// The primary category is what the URI is built from and what the template
	// cascade is searched from, so a document filed nowhere has neither.
	filings, err := p.db.CategoriesForDocument(ctx, doc.ID)
	if err != nil {
		return Result{}, err
	}
	primary, filed := domain.PrimaryOf(filings)
	if !filed {
		return Result{}, fmt.Errorf(
			"document %s is not filed in any category, so there is no address to publish it at: %w",
			doc.UID, domain.ErrConflict)
	}

	channels, err := p.channels(ctx, doc, payload.ChannelIDs)
	if err != nil {
		return Result{}, err
	}

	// Everything is rendered before anything is written and before the
	// transaction opens. render.Render returns bytes and never writes
	// (DESIGN.md 8.4), so a template that fails on the third of four channels
	// leaves the tree exactly as it was.
	type page struct {
		resource domain.Resource
		body     []byte
	}
	pages := make([]page, 0, len(channels))
	channelIDs := make([]int64, 0, len(channels))
	for _, oc := range channels {
		channelIDs = append(channelIDs, oc.ID)

		uri, err := domain.BuildURI(doc, version, primary, oc)
		if err != nil {
			return Result{}, err
		}
		outputPath, err := domain.OutputPath(oc.FileURI(uri))
		if err != nil {
			return Result{}, err
		}
		result, err := p.renderer.Render(render.Input{
			Mode:     domain.ModePublish,
			Site:     site,
			Channel:  oc,
			Category: primary,
			Document: doc,
			Version:  version,
			URI:      uri,
			URL:      oc.URL(site.Domain, uri),
		})
		if err != nil {
			return Result{}, err
		}
		sum := sha256.Sum256(result.Body)
		pages = append(pages, page{
			resource: domain.Resource{
				DocumentID:      doc.ID,
				OutputChannelID: oc.ID,
				VersionID:       version.ID,
				URI:             uri,
				Path:            outputPath,
				Checksum:        hex.EncodeToString(sum[:]),
				Bytes:           int64(len(result.Body)),
			},
			body: result.Body,
		})
	}

	now := p.now()
	resources := make([]domain.Resource, 0, len(pages))
	uris := make([]string, 0, len(pages))
	for _, pg := range pages {
		resources = append(resources, pg.resource)
		uris = append(uris, pg.resource.URI)
	}

	batch := p.tree.Begin()
	out, err := p.db.Publish(ctx, store.PublishRequest{
		DocumentID: doc.ID,
		VersionID:  version.ID,
		ChannelIDs: channelIDs,
		Resources:  resources,
		Now:        now,
		Expire: func(r domain.Resource) (store.NewJob, error) {
			return p.expireJob(r, payload.ActorID, now)
		},
		Write: func() error {
			for _, pg := range pages {
				if _, err := batch.Put(pg.resource.Path, pg.body); err != nil {
					return err
				}
			}
			return nil
		},
		Event: domain.Event{
			Type:    events.DocumentPublished,
			ActorID: payload.ActorID,
			Payload: map[string]any{
				"uid":      doc.UID,
				"version":  version.Number,
				"channels": channelNames(channels),
				"uris":     uris,
			},
			OccurredAt: now,
		},
	})
	if err != nil {
		// The rows are rolled back; the tree has to be put back by hand,
		// because a filesystem is not in the transaction. Rollback restores
		// what was there before every write this batch made, so a failure
		// leaves no partial output (PLAN.md M9 acceptance 6).
		if undo := batch.Rollback(); undo != nil {
			p.log.Error("undoing a failed publish",
				"document", doc.UID, "error", undo)
		}
		return Result{}, err
	}

	return Result{
		Document: doc,
		Version:  version,
		Written:  out.Written,
		Expired:  out.Expired,
		Jobs:     out.Jobs,
	}, nil
}

// Expire deletes a file the publisher no longer produces.
//
// The resource row is already gone: the publish that stopped producing the
// address deleted it in the transaction that scheduled this. So this job has
// only the filesystem to touch, and it is idempotent -- a file that is not
// there is the state the job wanted.
func (p *Publisher) Expire(ctx context.Context, payload domain.ExpirePayload) error {
	if err := payload.Validate(); err != nil {
		return err
	}
	if err := p.tree.Remove(payload.Path); err != nil {
		return err
	}

	// The event, because deleting a live page is a state change and every one
	// of them writes one (invariant 7). Its subject is the document rather
	// than the resource: the row it would name has been deleted, and an audit
	// trail whose subject can vanish is an audit trail with holes in it.
	if payload.DocumentID == 0 {
		return nil
	}
	_, err := p.db.RecordEvent(ctx, domain.Event{
		Type:        events.ResourceExpired,
		ActorID:     payload.ActorID,
		SubjectKind: domain.SubjectDocument,
		SubjectID:   payload.DocumentID,
		Payload: map[string]any{
			"uri":            payload.URI,
			"path":           payload.Path,
			"output_channel": payload.OutputChannelID,
		},
		OccurredAt: p.now(),
	})
	return err
}

// expireJob builds the job that deletes one stale address.
//
// The uid is minted here because a ULID encodes an instant and this is the
// layer holding a clock; internal/store has none (invariant 3).
func (p *Publisher) expireJob(r domain.Resource, actorID int64, now time.Time) (store.NewJob, error) {
	n, err := domain.ExpireJob(domain.ExpirePayload{
		DocumentID:      r.DocumentID,
		OutputChannelID: r.OutputChannelID,
		URI:             r.URI,
		Path:            r.Path,
		ActorID:         actorID,
	}, now)
	if err != nil {
		return store.NewJob{}, err
	}
	n = n.Normalize(now)
	uid, err := ids.New(now)
	if err != nil {
		return store.NewJob{}, err
	}
	return store.NewJob{
		UID: uid,
		Job: n,
		Event: domain.Event{
			Type:    events.JobEnqueued,
			ActorID: actorID,
			Payload: map[string]any{
				"uid":           uid,
				"kind":          n.Kind,
				"priority":      n.Priority,
				"scheduled_for": n.ScheduledFor,
				"max_attempts":  n.MaxAttempts,
			},
			OccurredAt: now,
		},
	}, nil
}

// channels resolves which output channels a publish covers.
//
// An empty list is every channel of the document's site, resolved now rather
// than when the job was scheduled: a channel added between the two is a
// channel the site wants, and a job scheduled for next Tuesday that silently
// ignored it would be a bug nobody could see.
func (p *Publisher) channels(ctx context.Context, doc domain.Document, ids []int64) ([]domain.OutputChannel, error) {
	if len(ids) == 0 {
		all, err := p.db.ListOutputChannels(ctx, doc.SiteID)
		if err != nil {
			return nil, err
		}
		if len(all) == 0 {
			return nil, fmt.Errorf(
				"site %d has no output channel, so nothing says what this document's address or file looks like: %w",
				doc.SiteID, domain.ErrConflict)
		}
		return all, nil
	}

	out := make([]domain.OutputChannel, 0, len(ids))
	for _, id := range ids {
		oc, err := p.db.OutputChannelByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if oc.SiteID != doc.SiteID {
			return nil, fmt.Errorf(
				"output channel %s is on site %d and document %s is on site %d: %w",
				oc.UID, oc.SiteID, doc.UID, doc.SiteID, domain.ErrInvalid)
		}
		out = append(out, oc)
	}
	return out, nil
}

func channelNames(channels []domain.OutputChannel) []string {
	out := make([]string, 0, len(channels))
	for _, oc := range channels {
		out = append(out, oc.Name)
	}
	return out
}
