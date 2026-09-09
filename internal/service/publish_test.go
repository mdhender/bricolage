// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/publish"
	"github.com/mdhender/bricolage/internal/render"
)

// M9's acceptance criteria, driven through the service and the job the service
// schedules (PLAN.md M9).
//
// The publish itself is a job, so every test here schedules one and then runs
// it, which is what the worker pool does with a claim in between. Running the
// handler directly rather than through a pool is deliberate: what these tests
// are about is what a publish does, and a pool between the schedule and the
// work would only add a race to the assertions. The pool's own behaviour is
// M6's and is tested there.

// publishHarness is a category tree, one output channel, one filed and
// checked-in document, a template tree, and an output tree.
type publishHarness struct {
	*catHarness
	tree      *publish.Tree
	publisher *publish.Publisher
	outputDir string
	doc       DocumentView
}

const publishTemplate = `<!doctype html><title>{{.Version.Title}}</title>
<p>version {{.Version.Number}}: {{raw (index .Content "body")}}</p>
<p>{{.URI}}</p>
`

func newPublishHarness(t *testing.T) *publishHarness {
	t.Helper()

	engine, err := render.New(render.Options{FS: fstest.MapFS{
		siteDomain + "/story.gohtml": &fstest.MapFile{Data: []byte(publishTemplate)},
	}, Reload: true})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	// t.TempDir already exists, which is the only reason the output tree needs
	// no mkdir here: publish.NewTree refuses to create its root, the same rule
	// --db lives under (invariant 19).
	outputDir := t.TempDir()
	tree, err := publish.NewTree(outputDir)
	if err != nil {
		t.Fatalf("publish.NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	h := newCatHarness(t)
	publisher, err := publish.New(publish.Options{
		DB: h.db, Renderer: engine, Tree: tree, Clock: h.clock,
	})
	if err != nil {
		t.Fatalf("publish.New: %v", err)
	}
	svc, err := New(h.db, Options{Clock: h.clock, Renderer: engine, Publisher: publisher})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	h.Service = svc

	if _, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
		SiteID: h.siteID, Name: "Web", UseSlug: true,
	}); err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}

	p := &publishHarness{catHarness: h, tree: tree, publisher: publisher, outputDir: outputDir}
	p.doc = p.story(t, "A Film Piece", "a-film-piece", "/features/film/")
	return p
}

// story creates a filed, checked-in document in a publishable state, which is
// the shape a publish needs: Publish refuses a state the workflow does not
// call publishable, and it pins a checked-in version.
func (h *publishHarness) story(t *testing.T, title, slug, path string) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          title,
		Slug:           slug,
		CoverDate:      "2026-03-01",
		Content:        `{"body":"draft one"}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument(%q): %v", title, err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{path}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	h.checkin(t, view.Document.UID, "first cut")
	return h.approve(t, view.Document.UID)
}

// approve walks a document to "approved", which is the first state the default
// workflow calls publishable.
func (h *publishHarness) approve(t *testing.T, uid string) DocumentView {
	t.Helper()
	if _, err := h.Transition(t.Context(), h.owner, uid, "review", ""); err != nil {
		t.Fatalf("Transition to review: %v", err)
	}
	view, err := h.Transition(t.Context(), h.owner, uid, "approved", "")
	if err != nil {
		t.Fatalf("Transition to approved: %v", err)
	}
	return view
}

// checkin takes the lease and gives it back, turning the open draft into a
// version. It is what has_checked_in_version asks for and what a publish pins.
func (h *publishHarness) checkin(t *testing.T, uid, note string) DocumentView {
	t.Helper()
	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	view, err := h.Checkin(t.Context(), h.owner, uid, note)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return view
}

// edit checks out, rewrites the body, and checks in again, producing a new
// version.
func (h *publishHarness) edit(t *testing.T, uid, body string) DocumentView {
	t.Helper()
	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.UpdateDraft(t.Context(), h.owner, uid, domain.DraftUpdate{
		Content: domain.Ref(`{"body":"` + body + `"}`),
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	view, err := h.Checkin(t.Context(), h.owner, uid, "edited")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return view
}

// drain claims and runs every job that is ready now, the way one worker with
// no other worker beside it would.
//
// It goes through Claim and Complete rather than reaching for the handler
// alone, because the claim is what enforces the schedule: a job scheduled for
// later is not claimable, which is what makes the pinning test a test of the
// clock rather than of the ordering of two statements. It returns the number
// of jobs that ran and the first handler failure, and stops at that failure --
// a test asserting that a refused publish left nothing behind must not have a
// second job run afterwards.
func (h *publishHarness) drain(t *testing.T) (int, error) {
	t.Helper()
	ran := 0
	for {
		job, ok, err := h.JobQueue().Claim(t.Context(), "test")
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if !ok {
			return ran, nil
		}
		if runErr := h.runJob(t.Context(), job); runErr != nil {
			if err := h.JobQueue().Fail(t.Context(), job, "test", runErr); err != nil {
				t.Fatalf("Fail: %v", err)
			}
			return ran, runErr
		}
		if err := h.JobQueue().Complete(t.Context(), job, "test"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		ran++
	}
}

// runDue runs every ready job and fails the test if any of them refuses.
func (h *publishHarness) runDue(t *testing.T) int {
	t.Helper()
	ran, err := h.drain(t)
	if err != nil {
		t.Fatalf("running a job: %v", err)
	}
	return ran
}

// runJob dispatches one job the way the registered handler would.
func (h *publishHarness) runJob(ctx context.Context, job domain.Job) error {
	switch job.Kind {
	case domain.KindPublish:
		payload, err := domain.ParsePublishPayload(job.Payload)
		if err != nil {
			return err
		}
		_, err = h.publisher.Publish(ctx, payload)
		return err
	case domain.KindExpire:
		payload, err := domain.ParseExpirePayload(job.Payload)
		if err != nil {
			return err
		}
		return h.publisher.Expire(ctx, payload)
	default:
		return nil
	}
}

// files lists the output tree, relative to its root.
func (h *publishHarness) files(t *testing.T) []string {
	t.Helper()
	got, err := h.tree.Files()
	if err != nil {
		t.Fatalf("walking the output tree: %v", err)
	}
	return got
}

// read returns the bytes at a path in the output tree.
func (h *publishHarness) read(t *testing.T, path string) string {
	t.Helper()
	body, err := h.tree.Read(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// TestPublishPinsTheVersion is PLAN.md M9 acceptance 1, the single most
// important test in the project.
//
// Schedule a version for T. Edit the document and check in a newer one.
// Advance the clock past T. What appears is the version that was scheduled,
// not the version that exists.
func TestPublishPinsTheVersion(t *testing.T) {
	h := newPublishHarness(t)
	uid := h.doc.Document.UID

	pinned := h.doc.Version.Number
	at := h.Now().Add(6 * time.Hour)

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: uid, At: at})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result.Version.Number != pinned {
		t.Fatalf("the job pinned version %d, want %d", result.Version.Number, pinned)
	}
	if !result.Job.ScheduledFor.Equal(at) {
		t.Errorf("scheduled for %v, want %v", result.Job.ScheduledFor, at)
	}

	// Nothing has happened yet: the job is in the future.
	if n := h.runDue(t); n != 0 {
		t.Fatalf("%d jobs ran before their scheduled time", n)
	}
	if got := h.files(t); len(got) != 0 {
		t.Fatalf("the output tree holds %v before the scheduled time", got)
	}

	// The document moves on while the job waits.
	newer := h.edit(t, uid, "the second draft")
	if newer.Version.Number <= pinned {
		t.Fatalf("the edit produced version %d, want one newer than %d", newer.Version.Number, pinned)
	}

	h.clock.Advance(7 * time.Hour)
	if n := h.runDue(t); n != 1 {
		t.Fatalf("%d jobs ran at the scheduled time, want the publish", n)
	}

	files := h.files(t)
	if len(files) != 1 {
		t.Fatalf("the output tree holds %v, want one file", files)
	}
	body := h.read(t, files[0])
	if !strings.Contains(body, "draft one") {
		t.Errorf("the published page is not the pinned version:\n%s", body)
	}
	if strings.Contains(body, "the second draft") {
		t.Errorf("the published page is the version that existed at the scheduled hour, not the one that was pinned:\n%s", body)
	}

	// And the row and the document agree about which version is live.
	_, resources, err := h.Resources(t.Context(), h.owner, uid)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 1 || resources[0].VersionID != result.Version.ID {
		t.Errorf("resources = %+v, want one row naming version %d", resources, result.Version.ID)
	}
	doc, err := h.Document(t.Context(), h.owner, uid)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Document.LiveVersionID != result.Version.ID {
		t.Errorf("live_version_id = %d, want the pinned version %d",
			doc.Document.LiveVersionID, result.Version.ID)
	}
}

// TestRepublishAfterASlugChangeExpiresTheOldAddress is PLAN.md M9
// acceptance 2, asserted on both the filesystem and published_resources.
func TestRepublishAfterASlugChangeExpiresTheOldAddress(t *testing.T) {
	h := newPublishHarness(t)
	uid := h.doc.Document.UID

	h.publishNow(t, uid)
	before := h.files(t)
	if len(before) != 1 || !strings.Contains(before[0], "a-film-piece") {
		t.Fatalf("the output tree holds %v, want the file at the original slug", before)
	}

	// The slug changes, which changes the address.
	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.UpdateDraft(t.Context(), h.owner, uid, domain.DraftUpdate{
		Slug: domain.Ref("a-different-piece"),
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	if _, err := h.Checkin(t.Context(), h.owner, uid, "renamed"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	h.publishNow(t, uid)

	// The expire job the republish scheduled has not run yet, so the old file
	// is still there and the row that claimed it is not.
	_, resources, err := h.Resources(t.Context(), h.owner, uid)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("published_resources holds %d rows, want one: %+v", len(resources), resources)
	}
	if !strings.Contains(resources[0].URI, "a-different-piece") {
		t.Errorf("the remaining row is at %q, want the new address", resources[0].URI)
	}

	h.runDue(t)
	after := h.files(t)
	if len(after) != 1 {
		t.Fatalf("the output tree holds %v after the expiry, want one file", after)
	}
	if !strings.Contains(after[0], "a-different-piece") {
		t.Errorf("the file left behind is %q, want the one at the new address", after[0])
	}
	if slices.Contains(after, before[0]) {
		t.Errorf("the file at the old address is still there: %v", after)
	}

	// The expiry wrote its event, because deleting a live page is a state
	// change (invariant 7).
	if got := h.eventsOfType(t, events.ResourceExpired); len(got) != 1 {
		t.Errorf("%d resource.expired events, want one", len(got))
	}
}

// TestRepublishAfterACategoryChangeExpiresTheOldAddress is PLAN.md M9
// acceptance 3. It is the same rule reached by moving the document rather than
// renaming it, which is the case a system that diffed on the slug alone would
// get wrong.
func TestRepublishAfterACategoryChangeExpiresTheOldAddress(t *testing.T) {
	h := newPublishHarness(t)
	uid := h.doc.Document.UID

	h.publishNow(t, uid)
	before := h.files(t)
	if len(before) != 1 || !strings.HasPrefix(before[0], "features/film/") {
		t.Fatalf("the output tree holds %v, want the file under features/film/", before)
	}

	if _, _, err := h.FileDocument(t.Context(), h.owner, uid, []string{"/business/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	h.publishNow(t, uid)
	h.runDue(t)

	after := h.files(t)
	if len(after) != 1 || !strings.HasPrefix(after[0], "business/") {
		t.Fatalf("the output tree holds %v, want one file under business/", after)
	}

	_, resources, err := h.Resources(t.Context(), h.owner, uid)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 1 || !strings.HasPrefix(resources[0].URI, "/business/") {
		t.Errorf("published_resources = %+v, want one row under /business/", resources)
	}
}

// TestTwoDocumentsAtOneAddressIsRefused is PLAN.md M9 acceptance 4: the
// collision is detected by result code and the refusal names the other
// document.
func TestTwoDocumentsAtOneAddressIsRefused(t *testing.T) {
	h := newPublishHarness(t)
	first := h.doc.Document.UID

	h.publishNow(t, first)
	before := h.files(t)

	// A second document with the same slug, cover date, and category resolves
	// to the same URI.
	second := h.story(t, "A Clash", "a-film-piece", "/features/film/")

	if _, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: second.Document.UID}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	_, err := h.drain(t)
	if err == nil {
		t.Fatal("two documents were published at one address")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the refusal is %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), first) {
		t.Errorf("the refusal is %q, want it to name the document that holds the address (%s)", err, first)
	}

	// And nothing of the second document reached the tree or the table
	// (PLAN.md M9 acceptance 6).
	if got := h.files(t); len(got) != len(before) {
		t.Errorf("the output tree holds %v after the refusal, want %v unchanged", got, before)
	}
	if got := h.read(t, before[0]); !strings.Contains(got, "A Film Piece") {
		t.Errorf("the first document's file was overwritten by the refused publish:\n%s", got)
	}
	_, resources, err := h.Resources(t.Context(), h.owner, second.Document.UID)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 0 {
		t.Errorf("the refused publish left %d resource rows behind", len(resources))
	}
	doc, err := h.Document(t.Context(), h.owner, second.Document.UID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Document.LiveVersionID != 0 {
		t.Errorf("live_version_id = %d after a failed publish, want it unchanged (PLAN.md M9 acceptance 5)",
			doc.Document.LiveVersionID)
	}
}

// TestAFailedRenderLeavesNothingBehind is PLAN.md M9 acceptance 6 reached the
// other way: the template runs out rather than the database refusing.
func TestAFailedRenderLeavesNothingBehind(t *testing.T) {
	h := newPublishHarness(t)

	// A document whose element type has no template anywhere. The lookup
	// fails, so nothing is rendered and nothing is written.
	if _, err := h.CreateElementType(t.Context(), h.owner, NewElementTypeRequest{
		KeyName: "note", Name: "Note", Kind: domain.KindStory, TopLevel: true,
		Schema: `{"fields":[{"name":"body","type":"block"}]}`,
	}); err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "note",
		Title: "A Note", Slug: "a-note", CoverDate: "2026-03-01", Content: `{"body":"words"}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{"/features/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	h.checkin(t, view.Document.UID, "ready")
	h.approve(t, view.Document.UID)

	if _, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: view.Document.UID}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := h.drain(t); err == nil {
		t.Fatal("a document with no template anywhere was published")
	} else if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the refusal is %v, want a missing template", err)
	}

	if got := h.files(t); len(got) != 0 {
		t.Errorf("a failed publish wrote %v", got)
	}
	_, resources, err := h.Resources(t.Context(), h.owner, view.Document.UID)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 0 {
		t.Errorf("a failed publish left %d resource rows behind", len(resources))
	}
}

// TestPublishWritesItsEvents is invariant 7 for the two operations M9 adds.
func TestPublishWritesItsEvents(t *testing.T) {
	h := newPublishHarness(t)
	uid := h.doc.Document.UID

	h.publishNow(t, uid)

	scheduled := h.eventsOfType(t, events.DocumentPublishScheduled)
	if len(scheduled) != 1 {
		t.Fatalf("%d publish_scheduled events, want one", len(scheduled))
	}
	if got := scheduled[0].Payload["version"]; got != float64(h.doc.Version.Number) {
		t.Errorf("the scheduled event records version %v, want %d; the pin is the promise being made",
			got, h.doc.Version.Number)
	}

	published := h.eventsOfType(t, events.DocumentPublished)
	if len(published) != 1 {
		t.Fatalf("%d published events, want one", len(published))
	}
	uris, ok := published[0].Payload["uris"].([]any)
	if !ok || len(uris) != 1 {
		t.Fatalf("the published event records uris %v, want one address", published[0].Payload["uris"])
	}
	if !strings.Contains(uris[0].(string), "a-film-piece") {
		t.Errorf("the published event records %v, want the document's address", uris[0])
	}
}

// TestPublishNeedsThePublishPrivilege is DESIGN.md 7's scale at the top.
func TestPublishNeedsThePublishPrivilege(t *testing.T) {
	h := newPublishHarness(t)
	writer := h.admin(t, "writer@example.com", "correct horse battery", domain.Edit)

	_, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: h.doc.Document.UID})
	if err != nil {
		t.Fatalf("the owner may publish: %v", err)
	}
	if _, err := h.Publish(t.Context(), writer, PublishRequest{UID: h.doc.Document.UID}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a writer holding Edit published: %v", err)
	}

	// Reading what was published needs only Read, because what is at a
	// document's addresses is part of the document.
	if _, _, err := h.Resources(t.Context(), writer, h.doc.Document.UID); err != nil {
		t.Errorf("a writer may not see where the document went: %v", err)
	}
}

// TestPublishRefusesAnUnpublishableState is the workflow saying what it means.
//
// A story in draft is not something the process is willing to put in front of
// readers, and a route that published it anyway would make the state machine
// advisory. The refusal is a conflict rather than a privilege failure, because
// it is a statement about the document and not about the person.
func TestPublishRefusesAnUnpublishableState(t *testing.T) {
	h := newPublishHarness(t)

	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
		Title: "Unfinished", Slug: "unfinished", CoverDate: "2026-03-01",
		Content: `{"body":"words"}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{"/features/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	h.checkin(t, view.Document.UID, "ready")

	_, err = h.Publish(t.Context(), h.owner, PublishRequest{UID: view.Document.UID})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("publishing a draft: %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "publishable") {
		t.Errorf("the refusal is %q, want it to say the state is not publishable", err)
	}
}

// TestPublishIsUnavailableWithoutAnOutputTree is the 503 of DESIGN.md 12's
// table: a well-formed request this process was not started to answer.
func TestPublishIsUnavailableWithoutAnOutputTree(t *testing.T) {
	h := newCatHarness(t)
	owner := h.owner

	view := h.filed(t, "A Story", "/features/")
	_, err := h.Publish(t.Context(), owner, PublishRequest{UID: view.Document.UID})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Publish on a server with no output tree: %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "--output") {
		t.Errorf("the refusal is %q, want it to name the flag that was not given", err)
	}
}

// TestReconcileFindsBothKindsOfOrphan is PLAN.md M9 acceptance 7.
func TestReconcileFindsBothKindsOfOrphan(t *testing.T) {
	h := newPublishHarness(t)
	h.publishNow(t, h.doc.Document.UID)

	diff, err := publish.Reconcile(t.Context(), h.db, h.tree)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !diff.Empty() {
		t.Fatalf("a freshly published tree disagrees with the database: %+v", diff)
	}

	// A row whose file is gone: somebody deleted it, and nothing will put it
	// back because a republish of an unchanged document produces the same
	// bytes at the same address.
	published := h.files(t)[0]
	if err := os.Remove(filepath.Join(h.outputDir, filepath.FromSlash(published))); err != nil {
		t.Fatalf("removing the published file: %v", err)
	}

	// A file nothing claims: it survives every slug change and every delete,
	// because no row names it for expiry.
	stray := filepath.Join(h.outputDir, "stray.html")
	if err := os.WriteFile(stray, []byte("nobody claims this"), 0o644); err != nil {
		t.Fatalf("writing a stray file: %v", err)
	}

	diff, err = publish.Reconcile(t.Context(), h.db, h.tree)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(diff.Missing) != 1 || diff.Missing[0] != published {
		t.Errorf("missing = %v, want %q", diff.Missing, published)
	}
	if len(diff.Unknown) != 1 || diff.Unknown[0] != "stray.html" {
		t.Errorf("unknown = %v, want stray.html", diff.Unknown)
	}
	if diff.Count() != 2 {
		t.Errorf("count = %d, want two discrepancies", diff.Count())
	}
}

// publishNow schedules a publish for the current instant and runs it, leaving
// the expire jobs it scheduled for the caller to run or to inspect.
func (h *publishHarness) publishNow(t *testing.T, uid string) PublishResult {
	t.Helper()
	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: uid})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	job, err := h.Job(t.Context(), h.owner, result.Job.UID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	claimed, ok, err := h.JobQueue().Claim(t.Context(), "test")
	if err != nil || !ok {
		t.Fatalf("Claim: %v, ok=%v", err, ok)
	}
	if claimed.UID != job.UID {
		t.Fatalf("claimed job %s, want the publish %s", claimed.UID, job.UID)
	}
	if err := h.runJob(t.Context(), claimed); err != nil {
		t.Fatalf("running the publish job: %v", err)
	}
	if err := h.JobQueue().Complete(t.Context(), claimed, "test"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return result
}
