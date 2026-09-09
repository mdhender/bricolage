// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/publish"
)

// M10's acceptance criteria, driven through the service and the jobs it
// schedules (PLAN.md M10).
//
// The traversal itself is tested exhaustively without a database in
// internal/publish; what is tested here is everything a pure function cannot
// see -- that the references are read from the version a publish would pin,
// that the policy decides what it says it decides, that a dry run writes
// nothing at all, and that five documents produce five files.

// relatedHarness is a publishHarness whose service carries a chosen
// related-asset policy.
type relatedHarness struct {
	*publishHarness
	policy config.RelatedFailure
}

func newRelatedHarness(t *testing.T, policy config.RelatedFailure) *relatedHarness {
	t.Helper()
	h := newPublishHarness(t)

	// The same database, the same clock, the same publisher; only the policy
	// differs. Rebuilding the service rather than reaching into it is what
	// keeps config.RelatedFailure a resolved setting rather than a mutable
	// field somebody could change halfway through a request.
	svc, err := New(h.db, Options{
		Clock:          h.clock,
		Publisher:      h.publisher,
		RelatedFailure: policy,
	})
	if err != nil {
		t.Fatalf("service.New(%q): %v", policy, err)
	}
	h.Service = svc
	return &relatedHarness{publishHarness: h, policy: policy}
}

// relatedContent renders the content of a story that references others.
func relatedContent(body string, refs ...string) string {
	payload := map[string]any{"body": body}
	if len(refs) > 0 {
		payload["related"] = refs
	}
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// storyRef creates a filed, checked-in, approved story whose content
// references the given documents by uid.
func (h *relatedHarness) storyRef(t *testing.T, title, slug, path string, refs ...string) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          title,
		Slug:           slug,
		CoverDate:      "2026-03-01",
		Content:        relatedContent("words", refs...),
	})
	if err != nil {
		t.Fatalf("CreateDocument(%q): %v", title, err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{path}); err != nil {
		t.Fatalf("FileDocument(%q): %v", title, err)
	}
	h.checkin(t, view.Document.UID, "first cut")
	return h.approve(t, view.Document.UID)
}

// draftRef creates a filed, checked-in story that is left in its initial
// state, which the default workflow does not call publishable.
func (h *relatedHarness) draftRef(t *testing.T, title, slug, path string, refs ...string) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          title,
		Slug:           slug,
		CoverDate:      "2026-03-01",
		Content:        relatedContent("words", refs...),
	})
	if err != nil {
		t.Fatalf("CreateDocument(%q): %v", title, err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{path}); err != nil {
		t.Fatalf("FileDocument(%q): %v", title, err)
	}
	return h.checkin(t, view.Document.UID, "first cut")
}

// pointAt rewrites a checked-in document's content to reference others,
// producing a new checked-in version. It is how a cycle is built: A cannot
// name B until B exists, and B cannot exist naming A unless A does.
func (h *relatedHarness) pointAt(t *testing.T, uid string, refs ...string) DocumentView {
	t.Helper()
	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.UpdateDraft(t.Context(), h.owner, uid, domain.DraftUpdate{
		Content: domain.Ref(relatedContent("words", refs...)),
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	view, err := h.Checkin(t.Context(), h.owner, uid, "now with relatives")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return view
}

// scheduledUIDs is what a publish pinned, root first, as the uids and version
// numbers a report names.
func scheduledUIDs(result PublishResult) []string {
	out := make([]string, 0, len(result.Scheduled))
	for _, sp := range result.Scheduled {
		out = append(out, fmt.Sprintf("%s@%d", sp.Document.UID, sp.Version.Number))
	}
	return out
}

func refusalUIDs(refusals []domain.Refusal) []string {
	out := make([]string, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, fmt.Sprintf("%s:%s", r.UID, r.Reason))
	}
	return out
}

// queued is every job on the queue, whatever its schedule.
func (h *relatedHarness) queued(t *testing.T) []domain.Job {
	t.Helper()
	jobs, err := h.db.QueryJobs(t.Context(), domain.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("QueryJobs: %v", err)
	}
	return jobs
}

// hasEvent reports whether a document's history holds an event of one type.
func (h *relatedHarness) hasEvent(t *testing.T, documentID int64, eventType string) bool {
	t.Helper()
	for _, e := range h.docEvents(t, documentID) {
		if e.Type == eventType {
			return true
		}
	}
	return false
}

// TestPublishCascadesToReferencedDocuments is PLAN.md M10's ordinary path and
// the shape the rest of the criteria vary: publishing a document publishes
// what it references, each with its own pinned version, and each file appears.
func TestPublishCascadesToReferencedDocuments(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	rel := h.storyRef(t, "The Sidebar", "the-sidebar", "/features/film/")
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/", rel.Document.UID)

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := scheduledUIDs(result); !slices.Equal(got, []string{
		root.Document.UID + "@1", rel.Document.UID + "@1",
	}) {
		t.Errorf("scheduled %v, want the root then the sidebar, each at version 1", got)
	}
	if len(result.Refusals) != 0 {
		t.Errorf("refused %v", result.Refusals)
	}
	for _, sp := range result.Scheduled {
		if sp.Job.UID == "" {
			t.Errorf("%s was scheduled with no job", sp.Document.UID)
		}
	}

	if ran := h.runDue(t); ran != 2 {
		t.Errorf("%d jobs ran, want 2", ran)
	}
	if got := h.files(t); len(got) != 2 {
		t.Errorf("the output tree holds %v, want two files", got)
	}

	// The related document's own history says it went live and why, rather
	// than the fact living only in the root's event (invariant 7).
	if !h.hasEvent(t, rel.Document.ID, events.DocumentPublishScheduled) {
		t.Errorf("the related document's history holds no %s", events.DocumentPublishScheduled)
	}
	payload := h.lastPayload(t, rel.Document.ID, events.DocumentPublishScheduled)
	if payload["root"] != root.Document.UID {
		t.Errorf("the related document's event names root %v, want %s", payload["root"], root.Document.UID)
	}
}

// TestPublishCycleTerminatesAndPublishesEachOnce is PLAN.md M10 acceptance 1,
// through the database and the jobs.
func TestPublishCycleTerminatesAndPublishesEachOnce(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	a := h.storyRef(t, "A", "a", "/features/film/")
	b := h.storyRef(t, "B", "b", "/features/film/", a.Document.UID)
	h.pointAt(t, a.Document.UID, b.Document.UID)

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: a.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := scheduledUIDs(result); !slices.Equal(got, []string{
		a.Document.UID + "@2", b.Document.UID + "@1",
	}) {
		t.Errorf("scheduled %v, want A at its second version and B at its first, each once", got)
	}
	if ran := h.runDue(t); ran != 2 {
		t.Errorf("%d jobs ran, want 2", ran)
	}
	if got := h.files(t); len(got) != 2 {
		t.Errorf("the output tree holds %v, want two files", got)
	}
}

// TestPublishDepthOfFive is PLAN.md M10 acceptance 7.
func TestPublishDepthOfFive(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	e := h.storyRef(t, "E", "e", "/features/film/")
	d := h.storyRef(t, "D", "d", "/features/film/", e.Document.UID)
	c := h.storyRef(t, "C", "c", "/features/film/", d.Document.UID)
	b := h.storyRef(t, "B", "b", "/features/film/", c.Document.UID)
	a := h.storyRef(t, "A", "a", "/features/film/", b.Document.UID)

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: a.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(result.Scheduled) != 5 {
		t.Fatalf("scheduled %v, want all five", scheduledUIDs(result))
	}
	if ran := h.runDue(t); ran != 5 {
		t.Errorf("%d jobs ran, want 5", ran)
	}
	if got := h.files(t); len(got) != 5 {
		t.Errorf("the output tree holds %v, want five files", got)
	}
}

// TestPublishRefusesUnderFail is PLAN.md M10 acceptance 2 and 4: the refusal
// names the document, and under "fail" nothing at all is published -- not even
// the root, which is the point of the setting.
func TestPublishRefusesUnderFail(t *testing.T) {
	cases := []struct {
		name       string
		build      func(t *testing.T, h *relatedHarness) (root, refused string)
		wantReason domain.RefusalReason
	}{
		{
			name: "a related document in a non-publishable state",
			build: func(t *testing.T, h *relatedHarness) (string, string) {
				rel := h.draftRef(t, "Still Writing", "still-writing", "/features/film/")
				root := h.storyRef(t, "The Feature", "the-feature", "/features/film/", rel.Document.UID)
				return root.Document.UID, rel.Document.UID
			},
			wantReason: domain.RefusedState,
		},
		{
			name: "a checked-out related document",
			build: func(t *testing.T, h *relatedHarness) (string, string) {
				rel := h.storyRef(t, "The Sidebar", "the-sidebar", "/features/film/")
				root := h.storyRef(t, "The Feature", "the-feature", "/features/film/", rel.Document.UID)
				if _, err := h.Checkout(t.Context(), h.owner, rel.Document.UID); err != nil {
					t.Fatalf("Checkout: %v", err)
				}
				return root.Document.UID, rel.Document.UID
			},
			wantReason: domain.RefusedCheckedOut,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRelatedHarness(t, config.RelatedFailureFail)
			root, refused := tc.build(t, h)

			before := len(h.queued(t))
			_, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root})
			if err == nil {
				t.Fatal("Publish went ahead; under fail a refusal publishes nothing")
			}
			if !errors.Is(err, domain.ErrConflict) {
				t.Errorf("Publish = %v, want ErrConflict", err)
			}

			// Named, which is the whole of acceptance 2 and 4.
			if !strings.Contains(err.Error(), refused) {
				t.Errorf("the refusal is %q; it must name %s", err, refused)
			}
			refusals, ok := domain.RefusalsOf(err)
			if !ok || len(refusals) != 1 {
				t.Fatalf("RefusalsOf = %v, %v; want one refusal", refusals, ok)
			}
			if refusals[0].UID != refused || refusals[0].Reason != tc.wantReason {
				t.Errorf("refusal = %+v, want %s refused for %q", refusals[0], refused, tc.wantReason)
			}
			if refusals[0].Referrer != root {
				t.Errorf("refusal was referenced by %q, want %s", refusals[0].Referrer, root)
			}

			// Nothing at all: no job, no event, no file.
			if after := len(h.queued(t)); after != before {
				t.Errorf("the queue went from %d jobs to %d; a refused publish schedules none", before, after)
			}
			if h.hasEvent(t, h.documentID(t, root), events.DocumentPublishScheduled) {
				t.Error("a refused publish wrote a publish event; under fail it writes none")
			}
			if ran := h.runDue(t); ran != 0 {
				t.Errorf("%d jobs ran, want none", ran)
			}
			if got := h.files(t); len(got) != 0 {
				t.Errorf("the output tree holds %v, want nothing", got)
			}
		})
	}
}

// TestPublishRefusesADocumentTheActorCannotPublish is PLAN.md M10
// acceptance 2's own case: the per-node permission check.
//
// The actor holds Publish over /features/ and its subtree and nothing else, so
// the root in /features/film/ is theirs to publish and the sidebar in
// /business/ is not. That is DESIGN.md 8.2's "permission-checked per node, not
// just the root", and it is the check the system we learned from performed in
// the template that drew the menu.
func TestPublishRefusesADocumentTheActorCannotPublish(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	rel := h.storyRef(t, "The Business Sidebar", "the-business-sidebar", "/business/")
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/", rel.Document.UID)

	features := h.cats["/features/"]
	editor := h.userWithGrant(t, "features@example.com", "correct horse battery", domain.Grant{
		Privilege: domain.Publish,
		Scope: domain.Scope{
			CategoryID:   domain.Ref(features.ID),
			CategoryPath: domain.Ref(features.Path),
			CategoryDeep: true,
		},
	})

	_, err := h.Publish(t.Context(), editor, PublishRequest{UID: root.Document.UID})
	if err == nil {
		t.Fatal("Publish went ahead; the actor may not publish the document it references")
	}
	refusals, ok := domain.RefusalsOf(err)
	if !ok || len(refusals) != 1 {
		t.Fatalf("RefusalsOf = %v, %v; want one refusal", refusals, ok)
	}
	if refusals[0].UID != rel.Document.UID {
		t.Errorf("refusal names %q, want %s", refusals[0].UID, rel.Document.UID)
	}
	if refusals[0].Reason != domain.RefusedPermission {
		t.Errorf("reason = %q, want %q", refusals[0].Reason, domain.RefusedPermission)
	}
	if got := h.files(t); len(got) != 0 {
		t.Errorf("the output tree holds %v, want nothing", got)
	}
}

// TestPublishWarnsAndGoesAhead is PLAN.md M10 acceptance 3: under "warn" the
// root publishes and the refusal is reported.
func TestPublishWarnsAndGoesAhead(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureWarn)

	blocked := h.draftRef(t, "Still Writing", "still-writing", "/features/film/")
	ok := h.storyRef(t, "The Sidebar", "the-sidebar", "/features/film/")
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/",
		blocked.Document.UID, ok.Document.UID)

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := scheduledUIDs(result); !slices.Equal(got, []string{
		root.Document.UID + "@1", ok.Document.UID + "@1",
	}) {
		t.Errorf("scheduled %v, want the root and the sidebar", got)
	}
	if got := refusalUIDs(result.Refusals); !slices.Equal(got,
		[]string{blocked.Document.UID + ":" + string(domain.RefusedState)}) {
		t.Errorf("refusals = %v, want the unfinished story named", got)
	}

	if ran := h.runDue(t); ran != 2 {
		t.Errorf("%d jobs ran, want 2", ran)
	}
	if got := h.files(t); len(got) != 2 {
		t.Errorf("the output tree holds %v, want two files", got)
	}

	// The refusal reaches the root's history as well as the response. The
	// question "why is that page still the old one" is asked the morning
	// after, when the response is long gone.
	payload := h.lastPayload(t, root.Document.ID, events.DocumentPublishScheduled)
	recorded, ok2 := payload["refusals"].([]any)
	if !ok2 || len(recorded) != 1 {
		t.Fatalf("the event payload's refusals are %v, want one", payload["refusals"])
	}
	entry, _ := recorded[0].(map[string]any)
	if entry["uid"] != blocked.Document.UID {
		t.Errorf("the recorded refusal names %v, want %s", entry["uid"], blocked.Document.UID)
	}
}

// TestDryRunMatchesTheRealPublish is PLAN.md M10 acceptance 6, asserted the
// way the criterion asks: run both and compare.
//
// The dry run goes first, because a dry run that ran second could agree by
// having been told the answer. Nothing it does may be visible to the publish
// that follows -- no job, no event, no file -- which is the other half of the
// assertion.
func TestDryRunMatchesTheRealPublish(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureWarn)

	blocked := h.draftRef(t, "Still Writing", "still-writing", "/features/film/")
	deep := h.storyRef(t, "The Footnote", "the-footnote", "/features/film/")
	mid := h.storyRef(t, "The Sidebar", "the-sidebar", "/features/film/", deep.Document.UID)
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/",
		mid.Document.UID, blocked.Document.UID, "01NOSUCHDOCUMENT")

	dry, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID, DryRun: true})
	if err != nil {
		t.Fatalf("Publish --dry-run: %v", err)
	}
	if !dry.DryRun {
		t.Error("the result does not say it was a dry run")
	}

	// Nothing happened.
	if jobs := h.queued(t); len(jobs) != 0 {
		t.Errorf("a dry run scheduled %d jobs, want none", len(jobs))
	}
	if h.hasEvent(t, root.Document.ID, events.DocumentPublishScheduled) {
		t.Error("a dry run wrote a publish event; it writes none")
	}
	for _, sp := range dry.Scheduled {
		if sp.Job.UID != "" {
			t.Errorf("a dry run reported job %q for %s", sp.Job.UID, sp.Document.UID)
		}
	}

	real, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if !slices.Equal(scheduledUIDs(dry), scheduledUIDs(real)) {
		t.Errorf("the dry run said %v and the publish did %v", scheduledUIDs(dry), scheduledUIDs(real))
	}
	if !slices.Equal(refusalUIDs(dry.Refusals), refusalUIDs(real.Refusals)) {
		t.Errorf("the dry run refused %v and the publish refused %v",
			refusalUIDs(dry.Refusals), refusalUIDs(real.Refusals))
	}
	if len(dry.Refusals) != 2 {
		t.Errorf("refusals = %v, want the unfinished story and the missing uid", refusalUIDs(dry.Refusals))
	}
	if got := scheduledUIDs(dry); len(got) != 3 {
		t.Errorf("scheduled %v, want the root, the sidebar, and the footnote", got)
	}
}

// TestDryRunReportsWhatFailWouldRefuse is the dry run's other half. Under
// "fail" a real publish is an error, and a dry run that answered with the same
// error would leave the caller no report to read -- which is the one thing a
// dry run exists to produce.
func TestDryRunReportsWhatFailWouldRefuse(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	blocked := h.draftRef(t, "Still Writing", "still-writing", "/features/film/")
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/", blocked.Document.UID)

	dry, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID, DryRun: true})
	if err != nil {
		t.Fatalf("Publish --dry-run: %v", err)
	}
	if !dry.WouldRefuse {
		t.Error("the dry run did not say the policy would refuse")
	}
	if len(dry.Scheduled) != 0 {
		t.Errorf("the dry run planned %v; under fail nothing at all is published", scheduledUIDs(dry))
	}
	if got := refusalUIDs(dry.Refusals); !slices.Equal(got,
		[]string{blocked.Document.UID + ":" + string(domain.RefusedState)}) {
		t.Errorf("refusals = %v", got)
	}

	// And the real publish agrees, with the same refusal in its error.
	_, err = h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID})
	refusals, ok := domain.RefusalsOf(err)
	if !ok {
		t.Fatalf("Publish = %v, want a refused cascade", err)
	}
	if !slices.Equal(refusalUIDs(refusals), refusalUIDs(dry.Refusals)) {
		t.Errorf("the publish refused %v and the dry run said %v",
			refusalUIDs(refusals), refusalUIDs(dry.Refusals))
	}
}

// TestCascadeReadsTheCheckedInVersion is invariant 8 applied to the reference
// list. A relative added to the working draft is not published tonight, for
// exactly the reason a body edited in the draft is not: what ships is what was
// checked in.
func TestCascadeReadsTheCheckedInVersion(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	rel := h.storyRef(t, "The Sidebar", "the-sidebar", "/features/film/")
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/")

	// Named in the open working draft and nowhere else.
	if _, err := h.Checkout(t.Context(), h.owner, root.Document.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.UpdateDraft(t.Context(), h.owner, root.Document.UID, domain.DraftUpdate{
		Content: domain.Ref(relatedContent("words", rel.Document.UID)),
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}

	result, err := h.Publish(t.Context(), h.owner, PublishRequest{UID: root.Document.UID})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := scheduledUIDs(result); !slices.Equal(got, []string{root.Document.UID + "@1"}) {
		t.Errorf("scheduled %v, want the root alone; the reference is only in the draft", got)
	}
}

// TestGraphIsLoadedFromTheDatabase covers the loader against a real database
// rather than the traversal: the references it finds, the version it reads
// them from, and the publishable bit it resolves from each document's own
// workflow.
func TestGraphIsLoadedFromTheDatabase(t *testing.T) {
	h := newRelatedHarness(t, config.RelatedFailureFail)

	deep := h.storyRef(t, "The Footnote", "the-footnote", "/features/film/")
	mid := h.draftRef(t, "The Sidebar", "the-sidebar", "/features/film/", deep.Document.UID)
	root := h.storyRef(t, "The Feature", "the-feature", "/features/film/",
		mid.Document.UID, "01NOSUCHDOCUMENT")

	graph, err := publish.LoadGraph(t.Context(), h.db, root.Document.ID)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}

	// Every reachable document is loaded, including the one Gather will
	// refuse and the one below it: the loader decides nothing.
	if len(graph.Nodes) != 3 {
		t.Errorf("the graph holds %d nodes, want the root, the sidebar, and the footnote", len(graph.Nodes))
	}

	rootNode := graph.Nodes[root.Document.ID]
	if len(rootNode.Refs) != 2 {
		t.Fatalf("the root has %d references, want the sidebar and the missing uid", len(rootNode.Refs))
	}
	if rootNode.Refs[0].ID != mid.Document.ID {
		t.Errorf("the first reference resolves to %d, want %d", rootNode.Refs[0].ID, mid.Document.ID)
	}
	if rootNode.Refs[1].ID != 0 || rootNode.Refs[1].UID != "01NOSUCHDOCUMENT" {
		t.Errorf("the second reference is %+v, want the missing uid resolving to nothing", rootNode.Refs[1])
	}
	if !rootNode.Publishable {
		t.Error("the root is approved and the workflow calls that publishable")
	}
	if graph.Nodes[mid.Document.ID].Publishable {
		t.Error("the sidebar was never approved and must not be publishable")
	}
	if graph.Nodes[deep.Document.ID].Version.ID == 0 {
		t.Error("the footnote is checked in and its version should have been read")
	}
}

// TestAnUnrecognisedPolicyIsRefused keeps the config key and the process
// honest: a service that read a misspelled "warm" as "fail" would be a
// configuration file saying one thing while the system did another.
func TestAnUnrecognisedPolicyIsRefused(t *testing.T) {
	h := newPublishHarness(t)
	_, err := New(h.db, Options{Clock: h.clock, RelatedFailure: config.RelatedFailure("warm")})
	if err == nil {
		t.Fatal("service.New accepted an unrecognised related-failure policy")
	}
	if !strings.Contains(err.Error(), "publish.related_failure") {
		t.Errorf("error = %q, want it to name the setting", err)
	}
}

// TestDefaultPolicyIsFail is the fail-safe direction, asserted where it is
// resolved: a Service built with nothing said about it refuses rather than
// warns.
func TestDefaultPolicyIsFail(t *testing.T) {
	h := newPublishHarness(t)
	if got := h.RelatedFailure(); got != config.RelatedFailureFail {
		t.Errorf("the default policy is %q, want %q", got, config.RelatedFailureFail)
	}
}

// documentID reads a document's primary key, which the assertions above need
// to ask the events table about it.
func (h *relatedHarness) documentID(t *testing.T, uid string) int64 {
	t.Helper()
	doc, err := h.db.DocumentByUID(t.Context(), uid)
	if err != nil {
		t.Fatalf("DocumentByUID(%q): %v", uid, err)
	}
	return doc.ID
}

// lastPayload reads the payload of the newest event of one type about one
// document.
func (h *relatedHarness) lastPayload(t *testing.T, documentID int64, eventType string) map[string]any {
	t.Helper()
	for _, e := range h.docEvents(t, documentID) {
		if e.Type == eventType {
			return e.Payload
		}
	}
	t.Fatalf("no %s event about document %d", eventType, documentID)
	return nil
}
