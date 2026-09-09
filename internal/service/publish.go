// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// Publishing's use cases (DESIGN.md 8, PLAN.md M9).
//
// There are two: schedule a publish, and look at what was published. Doing the
// publish is not here -- it is a job, and internal/publish runs it -- and that
// is the whole point of the milestone. A publish scheduled for midnight and a
// publish asked for now are the same request with a different instant in it,
// which is only true if the request never does the work itself.
//
// What this layer decides is the two things a job cannot: whether the caller
// may publish this document, and which version the job will pin. Both are
// decided now, when the person is here to be told, rather than at the
// scheduled hour when nobody is.

// PublishRequest is somebody asking for a document to be published.
type PublishRequest struct {
	// UID is the document.
	UID string

	// At is when it should happen. The zero time means now, which is the
	// ordinary case: somebody pressed Publish.
	At time.Time

	// ChannelUIDs are the output channels to publish to. Empty means every
	// channel of the document's site, resolved when the job runs.
	ChannelUIDs []string
}

// PublishResult is a scheduled publish.
type PublishResult struct {
	View DocumentView

	// Version is the version the job pinned. It is the newest checked-in
	// version and never the open working draft, and it is returned because it
	// is the promise the caller is being made: this is what will appear,
	// whatever the document becomes in the meantime (invariant 8).
	Version domain.Version

	// Channels are the channels named, or empty for "every channel of the
	// site".
	Channels []domain.OutputChannel

	// Job is the scheduled work.
	Job domain.Job
}

// Publish schedules a publish of a document's newest checked-in version
// (PLAN.md M9 acceptance 1).
//
// It needs Publish over the document, which is the top of the scale
// (DESIGN.md 7) and, in a seeded database, the administrator alone.
//
// It requires the document to be in a publishable state. That is the workflow
// saying what it means: a story in draft is not something the process is
// willing to put in front of readers, and a route that published it anyway
// would make the state machine advisory. The refusal is a conflict rather than
// a privilege failure, because it is a statement about the document and not
// about the person -- the same distinction guards and privileges get in
// DESIGN.md 6.1.
//
// The version is pinned here, at the moment of the request. That is
// invariant 8 and it is the reason this method exists rather than a job kind
// the client could enqueue: an editor approves version 5 for midnight and
// keeps working, and at midnight version 5 publishes.
func (s *Service) Publish(ctx context.Context, actor domain.Identity, req PublishRequest) (PublishResult, error) {
	if s.publisher == nil {
		return PublishResult{}, fmt.Errorf(
			"this server was started without an output tree, so it publishes nothing; give cmsd --output DIR (and --templates DIR): %w",
			domain.ErrUnavailable)
	}

	doc, err := s.mayDo(ctx, actor, req.UID, domain.Publish)
	if err != nil {
		return PublishResult{}, err
	}
	if err := s.publishableState(ctx, doc); err != nil {
		return PublishResult{}, err
	}

	version, err := s.db.LatestCheckedInVersion(ctx, doc.ID)
	if err != nil {
		return PublishResult{}, fmt.Errorf(
			"document %s has no checked-in version to publish; check it in first: %w", doc.UID, domain.ErrConflict)
	}

	channels, err := s.publishChannels(ctx, doc, req.ChannelUIDs)
	if err != nil {
		return PublishResult{}, err
	}
	channelIDs := make([]int64, 0, len(channels))
	for _, oc := range channels {
		channelIDs = append(channelIDs, oc.ID)
	}

	now := s.Now()
	at := req.At.UTC()
	if at.IsZero() {
		at = now
	}

	job, err := domain.PublishJob(domain.PublishPayload{
		DocumentID: doc.ID,
		VersionID:  version.ID,
		ChannelIDs: channelIDs,
		ActorID:    actor.User.ID,
	}, at)
	if err != nil {
		return PublishResult{}, err
	}
	scheduled, err := s.queue.Enqueue(ctx, job)
	if err != nil {
		return PublishResult{}, err
	}

	// The event on the document, beside the job.enqueued the queue writes on
	// the job. They are two records of two different things: "this job was
	// scheduled" belongs to the queue, and "this document was promised to
	// readers at midnight, from version 5" belongs to the document's history,
	// which is where somebody will go looking when the wrong thing appears.
	if _, err := s.db.RecordEvent(ctx, domain.Event{
		Type:        events.DocumentPublishScheduled,
		ActorID:     actor.User.ID,
		SubjectKind: domain.SubjectDocument,
		SubjectID:   doc.ID,
		Payload: map[string]any{
			"uid":           doc.UID,
			"version":       version.Number,
			"job":           scheduled.UID,
			"scheduled_for": at,
			"channels":      channelUIDs(channels),
		},
		OccurredAt: now,
	}); err != nil {
		return PublishResult{}, err
	}

	view, err := s.viewOf(ctx, doc)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{View: view, Version: version, Channels: channels, Job: scheduled}, nil
}

// Resources returns every file the publisher has written for a document
// (DESIGN.md 8.3).
//
// It needs Read and nothing more. What is at a document's addresses is part of
// the document, in the same way its history is, and the person who may look at
// the story may look at where it went.
func (s *Service) Resources(ctx context.Context, actor domain.Identity, uid string) (DocumentView, []domain.Resource, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, nil, err
	}
	view, err := s.viewOf(ctx, doc)
	if err != nil {
		return DocumentView{}, nil, err
	}
	resources, err := s.db.ResourcesForDocument(ctx, doc.ID)
	if err != nil {
		return DocumentView{}, nil, err
	}
	return view, resources, nil
}

// PublishConfigured reports whether this server can publish at all. It is what
// the startup banner and the log line ask.
func (s *Service) PublishConfigured() bool { return s.publisher != nil }

// publishableState refuses a document whose workflow does not call its current
// state publishable.
func (s *Service) publishableState(ctx context.Context, doc domain.Document) error {
	w, err := s.db.WorkflowByID(ctx, doc.WorkflowID)
	if err != nil {
		return err
	}
	state, ok := w.State(doc.State)
	if !ok {
		return fmt.Errorf("document %s is in state %q, which workflow %q does not declare: %w",
			doc.UID, doc.State, w.Name, domain.ErrConflict)
	}
	if !state.Publishable {
		return fmt.Errorf("document %s is in %q, which workflow %q does not call a publishable state: %w",
			doc.UID, state.Name, w.Name, domain.ErrConflict)
	}
	return nil
}

// publishChannels resolves the named output channels, or none for "every
// channel of the site".
//
// Unlike a preview, an unnamed channel is not an error when the site has two:
// publishing a document means publishing it everywhere it goes, and the
// milestone's whole expiry model is per channel, so "all of them" is a
// meaningful and common request. A preview has to pick one because it produces
// one page for a person to look at.
func (s *Service) publishChannels(ctx context.Context, doc domain.Document, uids []string) ([]domain.OutputChannel, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	out := make([]domain.OutputChannel, 0, len(uids))
	for _, uid := range uids {
		oc, err := s.db.OutputChannelByUID(ctx, uid)
		if err != nil {
			return nil, err
		}
		if oc.SiteID != doc.SiteID {
			return nil, fmt.Errorf(
				"output channel %s is on site %d and document %s is on site %d: %w",
				uid, oc.SiteID, doc.UID, doc.SiteID, domain.ErrInvalid)
		}
		out = append(out, oc)
	}
	return out, nil
}

func channelUIDs(channels []domain.OutputChannel) []string {
	out := make([]string, 0, len(channels))
	for _, oc := range channels {
		out = append(out, oc.UID)
	}
	return out
}
