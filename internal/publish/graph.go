// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"context"
	"errors"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/store"
)

// Loading the graph Gather walks (DESIGN.md 8.2, PLAN.md M10).
//
// This is the half of the cascade that reads, and it is deliberately the
// stupid half. It decides nothing: it follows every reference it finds,
// including references from documents Gather is about to refuse, and it does
// not apply a single one of the six gates. The reason is the reason the gates
// are in a pure function at all -- there is one statement of the rule, and a
// loader that skipped a node "because it would be refused anyway" would be a
// second statement, made in a place with no test that could see it.
//
// Following references from a document that will be refused costs a query and
// buys the report its detail: a refusal names the document, and naming it
// means having loaded it.

// LoadGraph reads every document reachable from rootID by following the
// document references in each one's newest checked-in version.
//
// References are read from the checked-in version rather than from the working
// draft, because the checked-in version is what a publish pins (invariant 8).
// A relative added to the draft this morning is not published tonight, and
// that is the same promise the version pin makes about the body: what ships is
// what was checked in.
//
// A document with no checked-in version has no references, not an error. It is
// a node in the graph with a zero Version, which Gather refuses by name.
func LoadGraph(ctx context.Context, db *store.DB, rootID int64) (Graph, error) {
	if db == nil {
		return Graph{}, errors.New("publish: no database to load the graph from")
	}

	g := Graph{Nodes: map[int64]Node{}}
	workflows := map[int64]domain.Workflow{}
	types := map[string]domain.ElementType{}

	seen := map[int64]bool{rootID: true}
	queue := []int64{rootID}

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		doc, err := db.DocumentByID(ctx, id)
		if err != nil {
			return Graph{}, err
		}
		node := Node{Document: doc}

		// Whether the workflow calls this document's state publishable. The
		// workflows are cached because a cascade is usually one workflow
		// several times over, and reading the same state machine once per
		// node would be the query this loop is careful about.
		wf, ok := workflows[doc.WorkflowID]
		if !ok {
			if wf, err = db.WorkflowByID(ctx, doc.WorkflowID); err != nil {
				return Graph{}, err
			}
			workflows[doc.WorkflowID] = wf
		}
		if state, ok := wf.State(doc.State); ok {
			node.Publishable = state.Publishable
		}

		// The version a publish of this document would pin, and the content
		// the references are read from. Its absence is a fact about the
		// document rather than a failure to load one.
		version, err := db.LatestCheckedInVersion(ctx, doc.ID)
		switch {
		case err == nil:
			node.Version = version
		case errors.Is(err, domain.ErrNotFound):
			// Left zero. Gather refuses it by name.
		default:
			return Graph{}, err
		}

		if node.Version.ID != 0 {
			et, ok := types[doc.ElementTypeKey]
			if !ok {
				if et, err = db.ElementTypeByKeyName(ctx, doc.ElementTypeKey); err != nil {
					return Graph{}, err
				}
				types[doc.ElementTypeKey] = et
			}
			uids, err := domain.References(&et, node.Version.Content)
			if err != nil {
				return Graph{}, err
			}
			for _, uid := range uids {
				ref := Ref{UID: uid}
				rel, err := db.DocumentByUID(ctx, uid)
				switch {
				case err == nil:
					ref.ID = rel.ID
					if !seen[rel.ID] {
						seen[rel.ID] = true
						queue = append(queue, rel.ID)
					}
				case errors.Is(err, domain.ErrNotFound):
					// Left zero. Gather refuses it by uid.
				default:
					return Graph{}, err
				}
				node.Refs = append(node.Refs, ref)
			}
		}

		g.Nodes[id] = node
	}
	return g, nil
}
