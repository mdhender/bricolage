// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
)

// The transition routes (DESIGN.md 12, PLAN.md M4).
//
// Modelling transitions as a subresource is the point of the design: GET tells
// you what the state machine permits and why, POST performs one. It makes the
// workflow visible in the API instead of hiding it behind "PATCH state=", and
// there is deliberately no route in this package that sets a state directly.

// transitionResponse is one entry of the menu.
//
// Refused transitions are in the list, with a reason and, when a guard was
// what refused, its name. A UI can then grey out "Approve" and say "needs one
// more approval", which is far better than the action silently not existing --
// an action that vanishes teaches nobody anything.
type transitionResponse struct {
	To   string `json:"to"`
	Name string `json:"name"`

	// Privilege is what the move needs, by name rather than by number: the
	// API speaks names (invariant 10 in spirit, DESIGN.md 7).
	Privilege string `json:"privilege"`

	Permitted bool   `json:"permitted"`
	Reason    string `json:"reason,omitempty"`

	// Guard names the guard that refused, when one did. It is the same
	// extension member the problem document carries, so a client switches on
	// one vocabulary rather than two.
	Guard string `json:"guard,omitempty"`

	// NeedsNote reports that the transition declares note_required, so a
	// client should offer a note field rather than treating the refusal as
	// final.
	NeedsNote bool `json:"needs_note,omitempty"`

	// Guards and Effects are what the transition declares, so that an admin
	// screen can show the process without a second request.
	Guards  []string `json:"guards"`
	Effects []string `json:"effects"`
}

// transitionsResponse is GET /api/v1/documents/{uid}/transitions.
type transitionsResponse struct {
	State       string               `json:"state"`
	Transitions []transitionResponse `json:"transitions"`
}

func (h *Handler) listTransitions(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	doc, allowed, err := h.svc.Transitions(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	out := transitionsResponse{State: doc.State, Transitions: make([]transitionResponse, 0, len(allowed))}
	for _, a := range allowed {
		out.Transitions = append(out.Transitions, transitionResponse{
			To:        a.Transition.To,
			Name:      a.Transition.Name,
			Privilege: a.Transition.Privilege.String(),
			Permitted: a.Permitted,
			Reason:    a.Reason,
			Guard:     string(a.Guard),
			NeedsNote: a.NeedsNote,
			Guards:    a.Transition.GuardNames(),
			Effects:   a.Transition.EffectNames(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// transitionRequest is the body of POST /api/v1/documents/{uid}/transitions,
// which DESIGN.md 12 gives as {"to":"review","note":"..."}.
type transitionRequest struct {
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// doTransition performs one transition.
//
// A "to" that is not a declared transition from the current state is a 409,
// including for an administrator: a move the state machine does not contain is
// not a permission question, and answering it as one would suggest that a
// bigger grant would help (PLAN.md M4 acceptance 2).
func (h *Handler) doTransition(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req transitionRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	view, err := h.svc.Transition(r.Context(), identity, r.PathValue("uid"), req.To, req.Note)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// workflowResponse is one configured process.
type workflowResponse struct {
	UID          string                    `json:"uid"`
	Name         string                    `json:"name"`
	Kind         string                    `json:"kind"`
	InitialState string                    `json:"initial_state"`
	States       []workflowStateResponse   `json:"states"`
	Transitions  []workflowTransitionsItem `json:"transitions"`
}

type workflowStateResponse struct {
	Slug              string `json:"slug"`
	Name              string `json:"name"`
	Position          int    `json:"position"`
	Publishable       bool   `json:"publishable"`
	Terminal          bool   `json:"terminal"`
	RequiredApprovals int    `json:"required_approvals"`
}

type workflowTransitionsItem struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Name      string   `json:"name"`
	Privilege string   `json:"privilege"`
	Guards    []string `json:"guards"`
	Effects   []string `json:"effects"`
}

// listWorkflows is the read half of the /workflows administration surface
// DESIGN.md 12 names. Writing one is an admin screen that arrives with the
// milestone that has a process worth editing; reading is what a client needs
// to render a state machine it did not configure.
func (h *Handler) listWorkflows(w http.ResponseWriter, r *http.Request, _ domain.Identity) {
	got, err := h.svc.Workflows(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]workflowResponse, 0, len(got))
	for _, wf := range got {
		item := workflowResponse{
			UID:          wf.UID,
			Name:         wf.Name,
			Kind:         wf.Kind,
			InitialState: wf.InitialState,
			States:       make([]workflowStateResponse, 0, len(wf.States)),
			Transitions:  make([]workflowTransitionsItem, 0, len(wf.Transitions)),
		}
		for _, s := range wf.States {
			item.States = append(item.States, workflowStateResponse{
				Slug:              s.Slug,
				Name:              s.Name,
				Position:          s.Position,
				Publishable:       s.Publishable,
				Terminal:          s.Terminal,
				RequiredApprovals: s.RequiredApprovals,
			})
		}
		for _, t := range wf.Transitions {
			item.Transitions = append(item.Transitions, workflowTransitionsItem{
				From:      t.From,
				To:        t.To,
				Name:      t.Name,
				Privilege: t.Privilege.String(),
				Guards:    t.GuardNames(),
				Effects:   t.EffectNames(),
			})
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}
