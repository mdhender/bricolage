// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/publish"
)

// Publishing's use cases (DESIGN.md 8, PLAN.md M9 and M10).
//
// There are two: schedule a publish, and look at what was published. Doing the
// publish is not here -- it is a job, and internal/publish runs it -- and that
// is the whole point of the milestone. A publish scheduled for midnight and a
// publish asked for now are the same request with a different instant in it,
// which is only true if the request never does the work itself.
//
// What this layer decides is the three things a job cannot: whether the caller
// may publish this document, which version each job will pin, and which other
// documents have to go with it. All three are decided now, when the person is
// here to be told, rather than at the scheduled hour when nobody is.
//
// The third is M10's, and where it happens is the milestone's one real design
// decision. The cascade is gathered when the publish is *scheduled* and not
// when it runs, because DESIGN.md 8.2 asks for the gathered set to be
// "returned to the caller for confirmation before anything is scheduled" --
// which is what --dry-run is -- and because the per-node permission check is a
// question about the person making the request. A cascade resolved at midnight
// would be checking the privileges of somebody who went home, against a
// document that has since moved, and would have nobody to refuse to.

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

	// DryRun gathers the set and reports the refusals without scheduling
	// anything or writing an event (PLAN.md M10 acceptance 6).
	//
	// It is a field on the ordinary request rather than a method of its own
	// so that there is one code path: a dry run that went somewhere else
	// could not honestly claim to say what a real publish would do, which is
	// the only thing a dry run is for.
	DryRun bool
}

// ScheduledPublish is one document a publish request pinned: which document,
// which version, and the job that will render it.
//
// The version is here rather than derivable because it is the promise being
// made (invariant 8), and the job is zero on a dry run, which schedules none.
type ScheduledPublish struct {
	Document domain.Document
	Version  domain.Version
	Job      domain.Job
}

// PublishResult is a scheduled publish and everything it drags along.
type PublishResult struct {
	View DocumentView

	// Channels are the channels named, or empty for "every channel of the
	// site".
	Channels []domain.OutputChannel

	// Scheduled is what this request pinned, the root first and then the
	// documents it references in traversal order (DESIGN.md 8.2). Every
	// entry names its own version, because each document pins its own
	// newest checked-in one.
	//
	// Nothing here promises an order of *publication*. The jobs are enqueued
	// in this order and the queue claims by priority and schedule; a
	// guarantee that a relative appears before the page linking to it is one
	// the queue does not make, and writing it down here would be a promise
	// nothing keeps.
	Scheduled []ScheduledPublish

	// Refusals are the referenced documents that will not be published, each
	// named (PLAN.md M10 acceptance 2, 4, 5). Under the "fail" policy a
	// non-empty list means nothing was scheduled at all, and a real publish
	// returns a *domain.RelatedError rather than this; a dry run reports it
	// here, because saying what would happen is what it is for.
	Refusals []domain.Refusal

	// WouldRefuse is set on a dry run whose refusals the policy would abort
	// on. It is how a dry run says "and therefore nothing would be
	// published" without erroring, which would leave the caller no report to
	// read.
	WouldRefuse bool

	// DryRun echoes the request, so that a client rendering the result
	// cannot present a plan as an outcome.
	DryRun bool

	// At is the instant this publish was asked for, with "now" already
	// resolved against the service's clock. It is returned because a dry run
	// schedules no job to read it off, and a report that could not say when
	// the publish would happen is a report missing half the request.
	At time.Time
}

// Root is the document somebody asked to publish. It is Scheduled[0], because
// Gather returns the root first, and it is a method so that no caller has to
// know that.
func (r PublishResult) Root() ScheduledPublish {
	if len(r.Scheduled) == 0 {
		return ScheduledPublish{}
	}
	return r.Scheduled[0]
}

// Related is everything the cascade gathered beside the root.
func (r PublishResult) Related() []ScheduledPublish {
	if len(r.Scheduled) < 2 {
		return nil
	}
	return r.Scheduled[1:]
}

// Publish schedules a publish of a document's newest checked-in version, and
// of every document it references (PLAN.md M9 acceptance 1, PLAN.md M10).
//
// It needs Publish over the document, which is the top of the scale
// (DESIGN.md 7) and, in a seeded database, the administrator alone. Every
// document the cascade reaches needs it too, resolved per node: Publish on the
// root says nothing about the related story the cascade would put live beside
// it, and DESIGN.md 8.2 is emphatic about the difference.
//
// It requires the document to be in a publishable state. That is the workflow
// saying what it means: a story in draft is not something the process is
// willing to put in front of readers, and a route that published it anyway
// would make the state machine advisory. The refusal is a conflict rather than
// a privilege failure, because it is a statement about the document and not
// about the person -- the same distinction guards and privileges get in
// DESIGN.md 6.1.
//
// Every version is pinned here, at the moment of the request. That is
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

	// The neighbourhood, and then the traversal over it. The loader reads and
	// decides nothing; Gather decides and reads nothing (DESIGN.md 8.2).
	graph, err := publish.LoadGraph(ctx, s.db, doc.ID)
	if err != nil {
		return PublishResult{}, err
	}
	set, refusals := publish.Gather(graph, doc.ID, actor, now)

	// The root's own pin, which the graph already read. A document with no
	// checked-in version is refused here rather than by Gather, because the
	// root is the caller's own request and deserves the message that says
	// what to do about it.
	if len(set) == 0 || set[0].Version.ID == 0 {
		return PublishResult{}, fmt.Errorf(
			"document %s has no checked-in version to publish; check it in first: %w", doc.UID, domain.ErrConflict)
	}

	view, err := s.viewOf(ctx, doc)
	if err != nil {
		return PublishResult{}, err
	}
	result := PublishResult{
		View:      view,
		Channels:  channels,
		Refusals:  refusals,
		DryRun:    req.DryRun,
		At:        at,
		Scheduled: make([]ScheduledPublish, 0, len(set)),
	}

	// The policy (DESIGN.md 8.2, config.RelatedFailure). Under "fail" a single
	// refusal publishes nothing at all -- not even the root, which is the
	// point: a page that goes live linking to a document that did not is the
	// failure the policy exists to prevent, and publishing the root and
	// warning would be the other setting.
	aborts := len(refusals) > 0 && s.relatedFailure.Aborts()
	if aborts && !req.DryRun {
		return PublishResult{}, &domain.RelatedError{UID: doc.UID, Refusals: refusals}
	}

	// A dry run stops here. It has gathered the same set over the same graph
	// with the same gates, which is what makes its report an answer about the
	// real publish rather than a second opinion (PLAN.md M10 acceptance 6).
	if req.DryRun {
		result.WouldRefuse = aborts
		if aborts {
			// Nothing would be scheduled, and a plan listing documents that
			// would not be published is a plan that lies.
			return result, nil
		}
		for _, node := range set {
			result.Scheduled = append(result.Scheduled, ScheduledPublish{
				Document: node.Document,
				Version:  node.Version,
			})
		}
		return result, nil
	}

	// One job per document, each pinning that document's own newest
	// checked-in version (invariant 8), all scheduled for the same instant.
	//
	// The channels named apply to the root. A relative on another site of its
	// own has nothing to do with them -- an output channel belongs to a site,
	// and handing one to a document on a different site is a refusal when the
	// job runs -- so such a relative is scheduled with an empty list, which
	// means "every channel of that document's site", resolved then. The
	// applied list is kept per document so that the events below say what was
	// actually asked for rather than what was asked for about the root.
	applied := make([][]string, 0, len(set))
	for i, node := range set {
		ids, named := channelIDs, channelUIDs(channels)
		if i > 0 && node.Document.SiteID != doc.SiteID {
			ids, named = nil, nil
		}
		job, err := domain.PublishJob(domain.PublishPayload{
			DocumentID: node.Document.ID,
			VersionID:  node.Version.ID,
			ChannelIDs: ids,
			ActorID:    actor.User.ID,
		}, at)
		if err != nil {
			return PublishResult{}, err
		}
		scheduled, err := s.queue.Enqueue(ctx, job)
		if err != nil {
			return PublishResult{}, err
		}
		result.Scheduled = append(result.Scheduled, ScheduledPublish{
			Document: node.Document,
			Version:  node.Version,
			Job:      scheduled,
		})
		applied = append(applied, named)
	}

	// The event on each document, beside the job.enqueued the queue writes on
	// each job. They are two records of two different things: "this job was
	// scheduled" belongs to the queue, and "this document was promised to
	// readers at midnight, from version 5" belongs to the document's history,
	// which is where somebody will go looking when the wrong thing appears.
	//
	// A related document gets one of its own rather than being a line in the
	// root's. Somebody reading its history and finding that it went live one
	// night should be able to see why from that history, and "because
	// something else referenced it" is the answer -- which is what the "root"
	// member says.
	rootUID := doc.UID
	for i, sp := range result.Scheduled {
		payload := map[string]any{
			"uid":           sp.Document.UID,
			"version":       sp.Version.Number,
			"job":           sp.Job.UID,
			"scheduled_for": at,
			"channels":      applied[i],
		}
		if i == 0 {
			if related := publish.UIDs(set)[1:]; len(related) > 0 {
				payload["related"] = related
			}
			if len(refusals) > 0 {
				payload["refusals"] = refusalPayload(refusals)
			}
		} else {
			payload["root"] = rootUID
		}
		if _, err := s.db.RecordEvent(ctx, domain.Event{
			Type:        events.DocumentPublishScheduled,
			ActorID:     actor.User.ID,
			SubjectKind: domain.SubjectDocument,
			SubjectID:   sp.Document.ID,
			Payload:     payload,
			OccurredAt:  now,
		}); err != nil {
			return PublishResult{}, err
		}
	}

	// Under "warn" the refusals reached the root's event above, and they
	// reach the log here as well: an editor sees them in the response, and
	// the operator who is asked "why is that page still the old one" tomorrow
	// morning has something to grep.
	if len(refusals) > 0 {
		for _, r := range refusals {
			s.log.Warn("a related document was not published",
				"root", rootUID, "document", r.UID, "reason", string(r.Reason), "detail", r.Detail)
		}
	}

	return result, nil
}

// refusalPayload renders refusals for an event payload.
//
// It is a slice of maps rather than the structs themselves because an event
// payload is JSON somebody reads years later, and a struct whose field names
// change would change the shape of history that was already written
// (invariant 7).
func refusalPayload(refusals []domain.Refusal) []map[string]any {
	out := make([]map[string]any, 0, len(refusals))
	for _, r := range refusals {
		entry := map[string]any{
			"uid":    r.UID,
			"reason": string(r.Reason),
			"detail": r.Detail,
		}
		if r.Referrer != "" {
			entry["referenced_by"] = r.Referrer
		}
		out = append(out, entry)
	}
	return out
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
