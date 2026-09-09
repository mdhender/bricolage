// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// The related-asset cascade's traversal (DESIGN.md 8.2, PLAN.md M10).
//
// Every one of these builds its graph by hand and touches no database, which
// is what DESIGN.md 8.2 asks for in so many words: "Implement the traversal as
// a pure function over a loaded graph so it can be tested without a database."
// The store-backed half is tested through internal/service, where the
// acceptance criteria are driven end to end.

var gatherNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// builder assembles a graph the way an editor assembles a package: documents
// with titles, and references from one to another.
type builder struct {
	t     *testing.T
	graph Graph
	byUID map[string]int64
	next  int64
}

func newBuilder(t *testing.T) *builder {
	t.Helper()
	return &builder{t: t, graph: Graph{Nodes: map[int64]Node{}}, byUID: map[string]int64{}, next: 1}
}

// nodeOpt tunes one document away from "publishable, checked in, and nobody's
// but the caller's", which is what a document a cascade may publish looks
// like.
type nodeOpt func(*Node)

// inState puts the document in a state its workflow does not call publishable.
func inState(state string) nodeOpt {
	return func(n *Node) {
		n.Document.State = state
		n.Publishable = false
	}
}

// checkedOutBy gives somebody a live edit lease on the document.
func checkedOutBy(userID int64) nodeOpt {
	return func(n *Node) {
		n.Document.Lock = domain.Lock{UserID: userID, ExpiresAt: gatherNow.Add(time.Hour)}
	}
}

// checkedOutAndExpired is the lease that ran out. It is not a lock, which is
// the whole point of a lease (DESIGN.md 5.1).
func checkedOutAndExpired(userID int64) nodeOpt {
	return func(n *Node) {
		n.Document.Lock = domain.Lock{UserID: userID, ExpiresAt: gatherNow.Add(-time.Minute)}
	}
}

// neverCheckedIn leaves the document with no version to pin.
func neverCheckedIn() nodeOpt {
	return func(n *Node) { n.Version = domain.Version{} }
}

// onSite moves the document to another site, which is the dimension the test
// grants are scoped by.
func onSite(siteID int64) nodeOpt {
	return func(n *Node) { n.Document.SiteID = siteID }
}

func (b *builder) add(uid, title string, opts ...nodeOpt) int64 {
	b.t.Helper()
	id := b.next
	b.next++
	node := Node{
		Document: domain.Document{
			ID: id, UID: uid, SiteID: 1, Kind: domain.KindStory,
			WorkflowID: 1, State: "approved",
		},
		Version:     domain.Version{ID: id * 100, DocumentID: id, Number: 1, Title: title},
		Publishable: true,
	}
	for _, opt := range opts {
		opt(&node)
	}
	b.graph.Nodes[id] = node
	b.byUID[uid] = id
	return id
}

// refs makes from's content name each uid in to, in that order. A uid with no
// document behind it is written as a bare string and resolves to zero, which
// is what a deleted relative looks like.
func (b *builder) refs(from string, to ...string) {
	b.t.Helper()
	id := b.byUID[from]
	node := b.graph.Nodes[id]
	for _, uid := range to {
		node.Refs = append(node.Refs, Ref{UID: uid, ID: b.byUID[uid]})
	}
	b.graph.Nodes[id] = node
}

func (b *builder) id(uid string) int64 { return b.byUID[uid] }

// publisher is an identity holding Publish over everything.
func publisher() domain.Identity {
	return domain.Identity{
		User:   domain.User{ID: 7},
		Grants: []domain.Grant{{ID: 1, RoleID: 1, Privilege: domain.Publish}},
	}
}

// publisherOfSite holds Publish over one site and nothing else, which is the
// ordinary shape of a real grant (DESIGN.md 7).
func publisherOfSite(siteID int64) domain.Identity {
	return domain.Identity{
		User: domain.User{ID: 7},
		Grants: []domain.Grant{{
			ID: 1, RoleID: 1, Privilege: domain.Publish,
			Scope: domain.Scope{SiteID: domain.Ref(siteID)},
		}},
	}
}

func uids(set []Node) []string { return UIDs(set) }

func refusedUIDs(refusals []domain.Refusal) []string {
	out := make([]string, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, r.UID)
	}
	return out
}

// TestGatherCycleTerminates is PLAN.md M10 acceptance 1. A references B
// references A: the traversal terminates and each document is gathered once.
//
// A test that only asserted the set would pass on a traversal that never
// terminated, so the assertion that matters is that this function returns at
// all -- which it does, or the test times out.
func TestGatherCycleTerminates(t *testing.T) {
	b := newBuilder(t)
	b.add("01A", "A")
	b.add("01B", "B")
	b.refs("01A", "01B")
	b.refs("01B", "01A")

	set, refusals := Gather(b.graph, b.id("01A"), publisher(), gatherNow)

	if got := uids(set); !slices.Equal(got, []string{"01A", "01B"}) {
		t.Errorf("gathered %v, want [01A 01B], each once", got)
	}
	if len(refusals) != 0 {
		t.Errorf("refused %v; a cycle is not a refusal", refusals)
	}
}

// TestGatherSelfReferenceTerminates is the smallest cycle there is, and the
// one a seen set seeded with the root has to get right.
func TestGatherSelfReferenceTerminates(t *testing.T) {
	b := newBuilder(t)
	b.add("01A", "A")
	b.refs("01A", "01A")

	set, refusals := Gather(b.graph, b.id("01A"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01A"}) {
		t.Errorf("gathered %v, want [01A] once", got)
	}
	if len(refusals) != 0 {
		t.Errorf("refused %v", refusals)
	}
}

// TestGatherDepth is PLAN.md M10 acceptance 7: a chain of five documents
// gathers all five.
func TestGatherDepth(t *testing.T) {
	b := newBuilder(t)
	for _, uid := range []string{"01A", "01B", "01C", "01D", "01E"} {
		b.add(uid, uid)
	}
	b.refs("01A", "01B")
	b.refs("01B", "01C")
	b.refs("01C", "01D")
	b.refs("01D", "01E")

	set, refusals := Gather(b.graph, b.id("01A"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01A", "01B", "01C", "01D", "01E"}) {
		t.Errorf("gathered %v, want all five", got)
	}
	if len(refusals) != 0 {
		t.Errorf("refused %v", refusals)
	}
}

// TestGatherDiamondGathersOnce covers the other shape a seen set is for: two
// documents referencing a third. Scheduling that third twice would render it
// twice and race two publishes at the same address.
func TestGatherDiamondGathersOnce(t *testing.T) {
	b := newBuilder(t)
	b.add("01A", "A")
	b.add("01B", "B")
	b.add("01C", "C")
	b.add("01D", "D")
	b.refs("01A", "01B", "01C")
	b.refs("01B", "01D")
	b.refs("01C", "01D")

	set, _ := Gather(b.graph, b.id("01A"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01A", "01B", "01C", "01D"}) {
		t.Errorf("gathered %v, want each of the four once, breadth first", got)
	}
}

// TestGatherRefusesByName is PLAN.md M10 acceptance 2, 4, and 5, and the
// missing-document case besides. Every refusal names the document, says which
// document referenced it, and gives a reason a client can switch on.
func TestGatherRefusesByName(t *testing.T) {
	tests := []struct {
		name       string
		opts       []nodeOpt
		actor      domain.Identity
		wantReason domain.RefusalReason
		wantDetail string
	}{
		{
			name:       "a related document the actor cannot publish",
			actor:      publisherOfSite(2),
			opts:       []nodeOpt{onSite(1)},
			wantReason: domain.RefusedPermission,
			wantDetail: "publish",
		},
		{
			name:       "a related document in a non-publishable state",
			actor:      publisher(),
			opts:       []nodeOpt{inState("draft")},
			wantReason: domain.RefusedState,
			wantDetail: "draft",
		},
		{
			name:       "a checked-out related document",
			actor:      publisher(),
			opts:       []nodeOpt{checkedOutBy(9)},
			wantReason: domain.RefusedCheckedOut,
			wantDetail: "checked out",
		},
		{
			name:       "a related document that has never been checked in",
			actor:      publisher(),
			opts:       []nodeOpt{neverCheckedIn()},
			wantReason: domain.RefusedNoVersion,
			wantDetail: "checked in",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder(t)
			// The root is on the site the actor holds a grant over in every
			// case, because the root's own permission is internal/service's
			// decision and not this function's.
			b.add("01ROOT", "The Root", onSite(siteOf(tc.actor)))
			b.add("01REL", "The Relative", tc.opts...)
			b.refs("01ROOT", "01REL")

			set, refusals := Gather(b.graph, b.id("01ROOT"), tc.actor, gatherNow)

			if got := uids(set); !slices.Equal(got, []string{"01ROOT"}) {
				t.Errorf("gathered %v, want the root alone", got)
			}
			if len(refusals) != 1 {
				t.Fatalf("refused %d documents, want 1: %v", len(refusals), refusals)
			}
			r := refusals[0]
			if r.UID != "01REL" {
				t.Errorf("refusal names %q, want 01REL", r.UID)
			}
			if r.Referrer != "01ROOT" {
				t.Errorf("refusal was referenced by %q, want 01ROOT", r.Referrer)
			}
			if r.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tc.wantReason)
			}
			if !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", r.Detail, tc.wantDetail)
			}
		})
	}
}

// siteOf reads the site a test identity's grant is scoped to, or 1 for an
// unscoped one.
func siteOf(actor domain.Identity) int64 {
	for _, g := range actor.Grants {
		if g.Scope.SiteID != nil {
			return *g.Scope.SiteID
		}
	}
	return 1
}

// TestGatherRefusesAMissingReference covers a uid in content that names no
// document. It is the ordinary way a reference goes stale: the related story
// was deleted after this version was checked in.
func TestGatherRefusesAMissingReference(t *testing.T) {
	b := newBuilder(t)
	b.add("01ROOT", "The Root")
	b.refs("01ROOT", "01GONE")

	set, refusals := Gather(b.graph, b.id("01ROOT"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01ROOT"}) {
		t.Errorf("gathered %v, want the root alone", got)
	}
	if len(refusals) != 1 || refusals[0].UID != "01GONE" {
		t.Fatalf("refusals = %v, want one naming 01GONE", refusals)
	}
	if refusals[0].Reason != domain.RefusedMissing {
		t.Errorf("reason = %q, want %q", refusals[0].Reason, domain.RefusedMissing)
	}
}

// TestGatherDoesNotTraverseThroughARefusal keeps a refused document's own
// relatives out of the set. Publishing them would schedule work for a page
// that will not appear.
func TestGatherDoesNotTraverseThroughARefusal(t *testing.T) {
	b := newBuilder(t)
	b.add("01ROOT", "The Root")
	b.add("01REL", "The Relative", inState("draft"))
	b.add("01DEEP", "Its Own Relative")
	b.refs("01ROOT", "01REL")
	b.refs("01REL", "01DEEP")

	set, refusals := Gather(b.graph, b.id("01ROOT"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01ROOT"}) {
		t.Errorf("gathered %v, want the root alone", got)
	}
	if got := refusedUIDs(refusals); !slices.Equal(got, []string{"01REL"}) {
		t.Errorf("refused %v, want 01REL alone; 01DEEP was never reached", got)
	}
}

// TestGatherIgnoresAnExpiredLease is the lease being a lease (DESIGN.md 5.1).
// An editor who closed their laptop must not block a publish until an
// administrator intervenes.
func TestGatherIgnoresAnExpiredLease(t *testing.T) {
	b := newBuilder(t)
	b.add("01ROOT", "The Root")
	b.add("01REL", "The Relative", checkedOutAndExpired(9))
	b.refs("01ROOT", "01REL")

	set, refusals := Gather(b.graph, b.id("01ROOT"), publisher(), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01ROOT", "01REL"}) {
		t.Errorf("gathered %v, want both; an expired lease is not a lock", got)
	}
	if len(refusals) != 0 {
		t.Errorf("refused %v", refusals)
	}
}

// TestGatherDoesNotGateTheRoot is the deliberate asymmetry documented on
// Gather. The root's permission and state are internal/service's decision,
// with the messages that name the privilege and the workflow; re-deciding them
// here would be a second statement of one rule. And the lock never applies to
// the root at all: publishing a document somebody has checked out publishes
// its newest checked-in version, which is how a correction goes out while the
// next edition is being written.
func TestGatherDoesNotGateTheRoot(t *testing.T) {
	b := newBuilder(t)
	b.add("01ROOT", "The Root", inState("draft"), checkedOutBy(9))

	set, refusals := Gather(b.graph, b.id("01ROOT"), publisherOfSite(99), gatherNow)
	if got := uids(set); !slices.Equal(got, []string{"01ROOT"}) {
		t.Errorf("gathered %v, want the root", got)
	}
	if len(refusals) != 0 {
		t.Errorf("refused %v; the root's own gates are the caller's", refusals)
	}
}

// TestGatherIsDeterministic is what PLAN.md M10 acceptance 6 rests on. The
// graph is a map, and a traversal that read it in map order would produce a
// different set every run -- so a dry run and a real publish could not be
// compared even when they agreed.
func TestGatherIsDeterministic(t *testing.T) {
	b := newBuilder(t)
	for _, uid := range []string{"01A", "01B", "01C", "01D", "01E", "01F"} {
		b.add(uid, uid)
	}
	b.add("01BAD", "Refused", inState("draft"))
	b.refs("01A", "01D", "01B", "01C", "01BAD")
	b.refs("01B", "01E")
	b.refs("01C", "01F")

	wantSet, wantRefusals := Gather(b.graph, b.id("01A"), publisher(), gatherNow)
	for i := range 50 {
		set, refusals := Gather(b.graph, b.id("01A"), publisher(), gatherNow)
		if !slices.Equal(uids(set), uids(wantSet)) {
			t.Fatalf("run %d gathered %v, want %v", i, uids(set), uids(wantSet))
		}
		if !slices.Equal(refusedUIDs(refusals), refusedUIDs(wantRefusals)) {
			t.Fatalf("run %d refused %v, want %v", i, refusedUIDs(refusals), refusedUIDs(wantRefusals))
		}
	}

	// Breadth first, in reference order: the root's own references before
	// their children, and 01D before 01B because the content named it first.
	if got := uids(wantSet); !slices.Equal(got, []string{"01A", "01D", "01B", "01C", "01E", "01F"}) {
		t.Errorf("gathered %v, want breadth first in reference order", got)
	}
}

// TestGatherWithoutItsRoot is the loader-bug case. A graph that does not
// contain the document it was asked about has nothing to say, and answering
// with a set built from a zero Node would schedule a publish of document 0.
func TestGatherWithoutItsRoot(t *testing.T) {
	set, refusals := Gather(Graph{Nodes: map[int64]Node{}}, 1, publisher(), gatherNow)
	if set != nil || refusals != nil {
		t.Errorf("Gather over an empty graph = %v, %v; want nothing", set, refusals)
	}
}
