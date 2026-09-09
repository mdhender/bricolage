// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
)

// Publishing's SQL, against a real in-memory database with all the migrations
// applied and foreign keys on (AGENTS.md, "Testing").
//
// What these tests are about is the transaction's shape: rows first, files
// last, and a failure anywhere rolling both back together.

// pubFixture is a document fixture with an output channel to publish into.
type pubFixture struct {
	*docFixture
	channelID int64
}

func newPubFixture(t *testing.T) *pubFixture {
	t.Helper()
	f := newDocFixture(t)

	oc, err := f.db.CreateOutputChannel(t.Context(), domain.OutputChannel{
		UID: ids.MustNew(f.now), SiteID: f.siteID, Name: "Web",
		Protocol:       domain.DefaultProtocol,
		Filename:       domain.DefaultFilename,
		FileExt:        domain.DefaultFileExt,
		URIFormat:      domain.DefaultURIFormat,
		FixedURIFormat: domain.DefaultFixedURIFormat,
		UseSlug:        true,
		URICase:        domain.URICaseLower,
	}, domain.Event{Type: events.OutputChannelCreated, OccurredAt: f.now})
	if err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}
	return &pubFixture{docFixture: f, channelID: oc.ID}
}

// publish records one document at one address, writing nothing but calling the
// callback the way a real publish does.
func (f *pubFixture) publish(t *testing.T, doc domain.Document, versionID int64, uris ...string) PublishResult {
	t.Helper()
	out, err := f.tryPublish(t, doc, versionID, uris...)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return out
}

func (f *pubFixture) tryPublish(t *testing.T, doc domain.Document, versionID int64, uris ...string) (PublishResult, error) {
	t.Helper()
	resources := make([]domain.Resource, 0, len(uris))
	for _, uri := range uris {
		resources = append(resources, domain.Resource{
			DocumentID:      doc.ID,
			OutputChannelID: f.channelID,
			VersionID:       versionID,
			URI:             uri,
			Path:            uri[1:] + "/index.html",
			Checksum:        "0000000000000000000000000000000000000000000000000000000000000000",
			Bytes:           int64(len(uri)),
		})
	}
	return f.db.Publish(t.Context(), PublishRequest{
		DocumentID: doc.ID,
		VersionID:  versionID,
		ChannelIDs: []int64{f.channelID},
		Resources:  resources,
		Now:        f.now,
		Expire:     f.expireJob,
		Write:      func() error { return nil },
		Event: domain.Event{
			Type: events.DocumentPublished, OccurredAt: f.now,
			Payload: map[string]any{"uid": doc.UID},
		},
	})
}

func (f *pubFixture) expireJob(r domain.Resource) (NewJob, error) {
	n, err := domain.ExpireJob(domain.ExpirePayload{
		DocumentID: r.DocumentID, OutputChannelID: r.OutputChannelID,
		URI: r.URI, Path: r.Path,
	}, f.now)
	if err != nil {
		return NewJob{}, err
	}
	return NewJob{
		UID: ids.MustNew(f.now),
		Job: n.Normalize(f.now),
		Event: domain.Event{
			Type: events.JobEnqueued, OccurredAt: f.now,
			Payload: map[string]any{"kind": n.Kind},
		},
	}, nil
}

// checkedIn turns the open draft into a version, because a publish pins one.
func (f *pubFixture) checkedIn(t *testing.T, doc domain.Document) (domain.Document, domain.Version) {
	t.Helper()
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	doc, v, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "ready",
		f.event(f.author.ID, events.DocumentCheckedIn))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return doc, v
}

// TestPublishRecordsResourcesAndMovesTheLiveVersion is the ordinary path, plus
// PLAN.md M9 acceptance 5.
func TestPublishRecordsResourcesAndMovesTheLiveVersion(t *testing.T) {
	f := newPubFixture(t)
	doc, _ := f.create(t, "A Story")
	doc, v := f.checkedIn(t, doc)

	out := f.publish(t, doc, v.ID, "/features/a-story")
	if len(out.Written) != 1 {
		t.Fatalf("Written = %+v, want one resource", out.Written)
	}
	got := out.Written[0]
	if got.URI != "/features/a-story" || got.VersionID != v.ID {
		t.Errorf("resource = %+v, want the address and the pinned version", got)
	}
	if got.DocumentUID != doc.UID || got.ChannelName != "Web" {
		t.Errorf("resource = %+v, want the document uid and the channel name joined in", got)
	}
	if len(out.Expired) != 0 || len(out.Jobs) != 0 {
		t.Errorf("a first publish expired %+v and scheduled %+v", out.Expired, out.Jobs)
	}

	after, err := f.db.DocumentByID(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if after.LiveVersionID != v.ID {
		t.Errorf("live_version_id = %d, want %d", after.LiveVersionID, v.ID)
	}

	// The event is in the same transaction (invariant 7).
	history, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(history) == 0 || history[0].Type != events.DocumentPublished {
		t.Errorf("the newest event is %+v, want document.published", history[0])
	}
}

// TestRepublishExpiresTheAddressesItNoLongerProduces is DESIGN.md 8.3's diff,
// with the expire jobs it schedules.
func TestRepublishExpiresTheAddressesItNoLongerProduces(t *testing.T) {
	f := newPubFixture(t)
	doc, _ := f.create(t, "A Story")
	doc, v := f.checkedIn(t, doc)

	f.publish(t, doc, v.ID, "/features/old", "/features/kept")

	out := f.publish(t, doc, v.ID, "/features/kept", "/features/new")
	if len(out.Expired) != 1 || out.Expired[0].URI != "/features/old" {
		t.Fatalf("Expired = %+v, want the address that is no longer produced", out.Expired)
	}
	if len(out.Jobs) != 1 || out.Jobs[0].Kind != domain.KindExpire {
		t.Fatalf("Jobs = %+v, want one expire job", out.Jobs)
	}
	// The job knows what to delete without reading a row, because by the time
	// it runs the row is gone.
	payload, err := domain.ParseExpirePayload(out.Jobs[0].Payload)
	if err != nil {
		t.Fatalf("the scheduled job's payload: %v", err)
	}
	if payload.Path != "features/old/index.html" {
		t.Errorf("the expire job deletes %q, want the old file", payload.Path)
	}

	rows, err := f.db.ResourcesForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("ResourcesForDocument: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("published_resources holds %d rows, want two", len(rows))
	}
	for _, r := range rows {
		if r.URI == "/features/old" {
			t.Errorf("the row for the expired address survived: %+v", r)
		}
	}
}

// TestTwoDocumentsAtOneAddressIsAConstraint is PLAN.md M9 acceptance 4 at the
// layer that detects it: a result code, never a message (invariant 11).
func TestTwoDocumentsAtOneAddressIsAConstraint(t *testing.T) {
	f := newPubFixture(t)

	first, _ := f.create(t, "First")
	first, v1 := f.checkedIn(t, first)
	second, _ := f.create(t, "Second")
	second, v2 := f.checkedIn(t, second)

	f.publish(t, first, v1.ID, "/features/clash")

	_, err := f.tryPublish(t, second, v2.ID, "/features/clash")
	if err == nil {
		t.Fatal("two documents were recorded at one address")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), first.UID) {
		t.Errorf("err = %q, want it to name the document that holds the address", err)
	}

	// The refusal left nothing of the second document behind
	// (PLAN.md M9 acceptance 6), and the first document's row is untouched.
	rows, err := f.db.ResourcesForDocument(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("ResourcesForDocument: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the refused publish left %d rows", len(rows))
	}
	held, err := f.db.ResourcesForDocument(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("ResourcesForDocument: %v", err)
	}
	if len(held) != 1 || held[0].URI != "/features/clash" || held[0].VersionID != v1.ID {
		t.Errorf("the address is held by %+v, want the first document's version", held)
	}
	after, err := f.db.DocumentByID(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if after.LiveVersionID != 0 {
		t.Errorf("live_version_id = %d after a failed publish, want it unchanged", after.LiveVersionID)
	}
}

// TestAFailedWriteRollsTheRowsBack is the callback's whole purpose: the files
// are written inside the transaction, so a write that fails takes the rows
// with it (PLAN.md M9 acceptance 6).
func TestAFailedWriteRollsTheRowsBack(t *testing.T) {
	f := newPubFixture(t)
	doc, _ := f.create(t, "A Story")
	doc, v := f.checkedIn(t, doc)

	// One good publish, so that there is something for the failed one to
	// expire and something for the rollback to restore.
	f.publish(t, doc, v.ID, "/features/old")

	boom := errors.New("the disk is full")
	_, err := f.db.Publish(t.Context(), PublishRequest{
		DocumentID: doc.ID,
		VersionID:  v.ID,
		ChannelIDs: []int64{f.channelID},
		Resources: []domain.Resource{{
			DocumentID: doc.ID, OutputChannelID: f.channelID, VersionID: v.ID,
			URI: "/features/new", Path: "features/new/index.html",
			Checksum: "0000000000000000000000000000000000000000000000000000000000000000",
		}},
		Now:    f.now,
		Expire: f.expireJob,
		Write:  func() error { return boom },
		Event:  domain.Event{Type: events.DocumentPublished, OccurredAt: f.now},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Publish = %v, want the write's own failure", err)
	}

	rows, err := f.db.ResourcesForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("ResourcesForDocument: %v", err)
	}
	if len(rows) != 1 || rows[0].URI != "/features/old" {
		t.Errorf("published_resources = %+v, want the previous publish untouched", rows)
	}

	// The expire job the failed publish would have scheduled is not there
	// either: a queue holding a job to delete a file that is still claimed is
	// how a page disappears for no reason anybody can find.
	jobs, err := f.db.QueryJobs(t.Context(), domain.JobFilter{Kind: domain.KindExpire})
	if err != nil {
		t.Fatalf("QueryJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("the rolled-back publish left %d expire jobs", len(jobs))
	}
}

// TestPublishNeedsAPin refuses the two shapes a caller could get wrong.
func TestPublishNeedsAPin(t *testing.T) {
	f := newPubFixture(t)
	doc, _ := f.create(t, "A Story")

	for _, tc := range []struct {
		name string
		req  PublishRequest
	}{
		{"no version", PublishRequest{DocumentID: doc.ID, ChannelIDs: []int64{f.channelID}, Write: func() error { return nil }}},
		{"no channel", PublishRequest{DocumentID: doc.ID, VersionID: 1, Write: func() error { return nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.db.Publish(t.Context(), tc.req); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("Publish = %v, want invalid", err)
			}
		})
	}
}

// TestLatestCheckedInVersionIsWhatAPublishPins. The current version is the
// open working draft while somebody holds the document, and publishing that
// would put unfinished work live.
func TestLatestCheckedInVersionIsWhatAPublishPins(t *testing.T) {
	f := newPubFixture(t)
	doc, _ := f.create(t, "A Story")

	// Nothing checked in yet.
	if _, err := f.db.LatestCheckedInVersion(t.Context(), doc.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("LatestCheckedInVersion on a document with only a draft = %v, want not found", err)
	}

	doc, v1 := f.checkedIn(t, doc)
	got, err := f.db.LatestCheckedInVersion(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("LatestCheckedInVersion: %v", err)
	}
	if got.ID != v1.ID {
		t.Errorf("pinned %d, want %d", got.ID, v1.ID)
	}

	// Check the document out again: the current version is now a new draft,
	// and the newest checked-in version is still version 1.
	doc, draft, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut))
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if doc.CurrentVersionID != draft.ID || !draft.IsDraft() {
		t.Fatalf("the checkout did not open a draft: %+v", draft)
	}
	got, err = f.db.LatestCheckedInVersion(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("LatestCheckedInVersion: %v", err)
	}
	if got.ID != v1.ID {
		t.Errorf("pinned %d while a draft was open, want the checked-in %d", got.ID, v1.ID)
	}
}
