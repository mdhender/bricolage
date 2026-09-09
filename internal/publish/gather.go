// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
)

// The related-asset cascade (DESIGN.md 8.2, PLAN.md M10).
//
// Publishing a document publishes the documents it references. Every bullet
// DESIGN.md 8.2 lists is a lesson somebody learned in production, and this
// file is all six of them in one function:
//
//   - cycle-safe, with a seen set keyed by document id and a work queue, so
//     that A referencing B referencing A terminates and publishes each once;
//   - permission-checked per node, because Publish on the root says nothing
//     about the related document the cascade would put live beside it;
//   - state-gated, because a related story still in draft is not something the
//     process is willing to show readers;
//   - lock-gated, because somebody is in the middle of writing it;
//   - reviewable, because the set is returned to the caller before anything is
//     scheduled -- which is what --dry-run is;
//   - policy-driven, which is the caller's decision and not this function's:
//     Gather reports, config.RelatedFailure decides.
//
// It is a pure function over a loaded graph, exactly as DESIGN.md 8.2 asks,
// so that every one of those can be tested without a database. LoadGraph, in
// graph.go, is the half that reads.

// Node is one document in the graph, with everything the gates need to decide
// about it.
//
// Version is the checked-in version a publish of this document would pin
// (invariant 8), and its zero value means there is none -- a document whose
// only version is its first open draft. Publishable is whether the document's
// own workflow calls its current state publishable; it is resolved by the
// loader rather than carried as a workflow here, because a pure function
// cannot go and read one and because the answer is one bit.
type Node struct {
	Document domain.Document
	Version  domain.Version

	Publishable bool

	// Refs are the documents this node's pinned content references, in the
	// order domain.References produced them. A Ref with an ID of zero is a
	// uid naming no document.
	Refs []Ref
}

// UID is the document's external identifier, which is what every refusal and
// every report names (invariant 10).
func (n Node) UID() string { return n.Document.UID }

// Title is the pinned version's title, or the empty string when there is no
// pinned version to have one.
func (n Node) Title() string { return n.Version.Title }

// Ref is one reference found in a node's content: the uid as it was written,
// and the document it resolves to.
//
// Both are carried because a uid that resolves to nothing is a refusal that
// has to name the uid, and a uid that resolves is a document the traversal
// reaches by id. A Ref that carried only the resolved document could not
// report the first case at all.
type Ref struct {
	UID string
	ID  int64
}

// Graph is the loaded neighbourhood of a publish: every document reachable
// from the root by following references, keyed by document id.
//
// It is a plain map rather than a type with a constructor because the point of
// it is that a test can build one by hand. The traversal below is the thing
// worth testing and it must be testable without a database
// (DESIGN.md 8.2).
type Graph struct {
	Nodes map[int64]Node
}

// Gather walks the graph from root and reports what would be published and
// what would not (PLAN.md M10).
//
// The returned set is in traversal order with the root first, and the refusals
// are in the order the traversal met them. Both orders are deterministic, and
// that is load-bearing rather than tidy: PLAN.md M10 acceptance 6 asserts that
// a dry run and a real publish produce the same answer by comparing them.
//
// The root is added to the set without being gated, and that is deliberate.
// The caller has already decided it: internal/service resolves Publish over
// the document and refuses a state the workflow does not call publishable,
// with messages that name the privilege and the workflow. Re-deciding it here
// would be a second statement of one rule, and two statements of one rule is
// how they come to disagree. What DESIGN.md 8.2 asks for is that the
// *relatives* are checked too -- "not just the root" -- and that is what this
// does.
//
// The one gate the root is deliberately exempt from even in spirit is the
// lock. Publishing a document somebody has checked out publishes its newest
// checked-in version, which is a coherent thing to ask for and the ordinary
// way a correction goes out while the next edition is being written. Doing
// that to a *relative* is different: nobody asked for it, and it would put
// somebody else's document live while they were in the middle of it.
//
// A refused node is not traversed through. Its own references are not gathered
// because it is not being published, and gathering them would schedule work
// for a page that will not appear.
func Gather(g Graph, root int64, actor domain.Identity, now time.Time) ([]Node, []domain.Refusal) {
	if _, ok := g.Nodes[root]; !ok {
		// A graph that does not contain its own root is a loader bug rather
		// than an editorial condition, and there is nothing to report about
		// a document this function was not given.
		return nil, nil
	}

	var (
		set      []Node
		refusals []domain.Refusal
	)
	seen := map[int64]bool{root: true}
	queue := []int64{root}

	for len(queue) > 0 {
		node := g.Nodes[queue[0]]
		queue = queue[1:]
		set = append(set, node)

		for _, ref := range node.Refs {
			if ref.ID == 0 {
				refusals = append(refusals, domain.Refusal{
					UID:      ref.UID,
					Referrer: node.UID(),
					Reason:   domain.RefusedMissing,
					Detail:   "no document has this uid; it may have been deleted since this version was written",
				})
				continue
			}
			if seen[ref.ID] {
				// The cycle guard, and also the diamond guard: two documents
				// referencing a third gather it once. Marked when a
				// reference is met rather than when the node is visited, so
				// that a document already queued is not queued twice.
				continue
			}
			seen[ref.ID] = true

			rel, ok := g.Nodes[ref.ID]
			if !ok {
				refusals = append(refusals, domain.Refusal{
					UID:      ref.UID,
					Referrer: node.UID(),
					Reason:   domain.RefusedMissing,
					Detail:   "this document could not be loaded",
				})
				continue
			}
			if refusal, refused := gate(rel, node, actor, now); refused {
				refusals = append(refusals, refusal)
				continue
			}
			queue = append(queue, ref.ID)
		}
	}
	return set, refusals
}

// gate applies the three checks DESIGN.md 8.2 names, plus the one invariant 8
// implies, to one related document.
//
// The order is not arbitrary. Permission comes first so that the rest of the
// answer is only ever given about a document the caller may publish: telling
// somebody that a document they have no privilege over is checked out by a
// named colleague is a disclosure, and it is one made in a message they did
// not need. Everything after it is a statement about the document, and the
// caller has already been shown to be entitled to it.
func gate(rel, referrer Node, actor domain.Identity, now time.Time) (domain.Refusal, bool) {
	refusal := domain.Refusal{
		UID:      rel.UID(),
		Title:    rel.Title(),
		Referrer: referrer.UID(),
	}

	switch {
	case !authz.Allows(actor.Grants, rel.Document.Subject(), domain.Publish):
		refusal.Reason = domain.RefusedPermission
		refusal.Detail = fmt.Sprintf("%s is required over this document and you do not hold it", domain.Publish)

	case rel.Document.Lock.Held(now):
		// Held by anybody, the caller included. A publish pins the newest
		// checked-in version, so publishing a document under a live lease
		// would put out a page while its author is still working on the next
		// one -- and the author having asked to publish something else is not
		// the same as having asked to publish this. One rule rather than two,
		// and the safe one.
		refusal.Reason = domain.RefusedCheckedOut
		refusal.Detail = "this document is checked out; publishing it now would put a page out from under whoever is editing it"

	case !rel.Publishable:
		refusal.Reason = domain.RefusedState
		refusal.Detail = fmt.Sprintf(
			"this document is in %q, which its workflow does not call a publishable state", rel.Document.State)

	case rel.Version.ID == 0:
		refusal.Reason = domain.RefusedNoVersion
		refusal.Detail = "this document has never been checked in, so there is no version to pin"

	default:
		return domain.Refusal{}, false
	}
	return refusal, true
}

// UIDs renders a gathered set as the uids a report and an event payload name.
func UIDs(set []Node) []string {
	out := make([]string, 0, len(set))
	for _, n := range set {
		out = append(out, n.UID())
	}
	return out
}
