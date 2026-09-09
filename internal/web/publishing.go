// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// Preview and publish (PLAN.md M8, M9, M10; M13's buttons for them).

// previewPage is what POST /documents/{uid}/preview renders.
//
// The rendered page itself is not shown here: it is written to the scratch
// tree and served at /preview/{name}, behind the same session, so what this
// screen shows is where it went, which template produced it, and what the
// lookup beat. "Which template did this use" is the question asked when the
// wrong one is used, and the answer is worthless without the list it won
// against (DESIGN.md 8.4).
type previewPage struct {
	Base

	Result service.PreviewResult

	// Path is where the preview is served, relative to this origin.
	Path string
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	result, err := h.svc.Preview(r.Context(), identity, service.PreviewRequest{
		UID:        r.PathValue("uid"),
		ChannelUID: r.PostFormValue("channel"),
		Validate:   r.PostFormValue("validate") != "",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := previewPage{Base: h.base(r, identity, "Preview"), Result: result}
	if result.Entry.Name != "" {
		p.Path = result.Entry.Path()
	}
	h.render(w, r, "preview.gohtml", http.StatusOK, p)
}

// publishPage is what POST /documents/{uid}/publications renders.
//
// A real publish answers 202 in the JSON API: nothing has been published, a
// job has been scheduled. The page says the same thing in words, and names the
// version each job pinned -- that is the promise invariant 8 makes, and a
// screen that could not show it would be asking an editor to take it on trust.
type publishPage struct {
	Base

	Result service.PublishResult
	Doc    domain.Document
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	at, err := parseWhen(r.PostFormValue("at"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	req := service.PublishRequest{
		UID:    r.PathValue("uid"),
		DryRun: r.PostFormValue("dry_run") != "",
	}
	if at != nil {
		req.At = *at
	}
	for _, raw := range r.PostForm["channel"] {
		if raw = strings.TrimSpace(raw); raw != "" {
			req.ChannelUIDs = append(req.ChannelUIDs, raw)
		}
	}

	result, err := h.svc.Publish(r.Context(), identity, req)
	if err != nil {
		// A cascade refused under publish.related_failure = fail is a 409
		// carrying the documents that held it up, and the error page draws
		// them: an editor who is told only "conflict" cannot go and fix it.
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "publish.gohtml", http.StatusOK, publishPage{
		Base:   h.base(r, identity, "Publish"),
		Result: result,
		Doc:    result.View.Document,
	})
}
