// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// Version history and the word-level diff.

// historyPage is GET /documents/{uid}/history.
type historyPage struct {
	Base

	Doc      domain.Document
	Versions []versionRow
	Events   []eventRow
}

type versionRow struct {
	Version domain.Version
	Author  string

	// Open marks the working draft, which is the version that is not yet a
	// version: it may still change, so it is drawn differently from the ones
	// that may not.
	Open bool

	// Live marks the version that is published.
	Live bool
}

type eventRow struct {
	Event domain.Event
	Actor string

	// Name is the event type's display name, taken from the vocabulary in
	// internal/events. DESIGN.md 10 says the screens list event types from
	// code rather than from a SELECT, and this is a screen.
	Name string
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	uid := r.PathValue("uid")
	doc, versions, err := h.svc.Versions(r.Context(), identity, uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	log, err := h.svc.DocumentEvents(r.Context(), identity, uid, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	who := h.lookup(r)
	p := historyPage{Base: h.base(r, identity, "History"), Doc: doc}
	for _, v := range versions {
		p.Versions = append(p.Versions, versionRow{
			Version: v,
			Author:  who(v.CreatedBy),
			Open:    v.CheckedInAt.IsZero(),
			Live:    v.ID != 0 && v.ID == doc.LiveVersionID,
		})
	}
	for _, e := range log {
		row := eventRow{Event: e, Name: events.Name(e.Type)}
		if e.ActorID != 0 {
			row.Actor = who(e.ActorID)
		}
		p.Events = append(p.Events, row)
	}
	h.render(w, r, "history.gohtml", http.StatusOK, p)
}

// diffPage is GET /documents/{uid}/diff?from=&to=.
type diffPage struct {
	Base

	Doc  domain.Document
	From domain.Version
	To   domain.Version

	Fields []domain.FieldDiff
}

// diff shows the word-level difference between two versions.
func (h *Handler) diff(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	from, err := intParam(r, "from", 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	to, err := intParam(r, "to", 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d, err := h.svc.Diff(r.Context(), identity, r.PathValue("uid"), from, to)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "diff.gohtml", http.StatusOK, diffPage{
		Base:   h.base(r, identity, "Diff"),
		Doc:    d.Document,
		From:   d.From,
		To:     d.To,
		Fields: d.Fields,
	})
}
