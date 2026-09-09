// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"io"
	"net/http"
	"strconv"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/service"
)

// Preview: rendering a document, and serving what was rendered
// (DESIGN.md 8.4, PLAN.md M8).
//
// Two routes, and only one of them is under /api/v1. Rendering is a POST to a
// subresource, like a transition or a checkout, because it produces something:
// a file in the scratch tree with an address of its own. Serving it back is a
// plain GET at /preview/{name}, because what comes back is a page and not a
// document about a page -- the whole point is that a browser can be pointed at
// it.
//
// The second route is registered separately, by internal/server, so that the
// route table can call it what it is. It lives in this package because it
// needs the same authentication the rest of the API has, and a second
// implementation of "who is asking" is how one of them ends up not asking.

// RegisterPreview adds the preview mount at /preview/.
//
// It is separate from Register because it is not under Prefix and because the
// route table names it separately. Like the API routes, it is registered even
// when there is no service behind it, so that "cmsd routes" prints the table
// "serve" would mount.
func RegisterPreview(mux Mux, deps Deps) {
	h := New(deps)
	if deps.Service == nil {
		mux.Handle("GET "+render.PreviewPrefix+"{name}", http.HandlerFunc(unavailable))
		return
	}
	mux.Handle("GET "+render.PreviewPrefix+"{name}", h.authenticated(h.servePreview))
}

// previewRequest is the body of POST /api/v1/documents/{uid}/preview.
type previewRequest struct {
	// Channel is the output channel's uid. It may be omitted when the site
	// has exactly one.
	Channel string `json:"channel,omitempty"`

	// Validate asks for validate mode: parse the template, report what is
	// wrong with it, and write nothing.
	Validate bool `json:"validate,omitempty"`
}

// previewResponse is a rendered preview as the API speaks it.
type previewResponse struct {
	UID     string `json:"uid"`
	Mode    string `json:"mode"`
	Channel string `json:"channel"`
	Name    string `json:"channel_name"`

	// Version is which version was rendered, and Draft says whether it was
	// the open working draft (PLAN.md M8 acceptance 4).
	Version int  `json:"version"`
	Draft   bool `json:"draft"`

	Category string `json:"category"`
	URI      string `json:"uri"`
	URL      string `json:"url"`

	// Template is the template that was used and Searched is every path that
	// was tried, deepest first. Searched is here because "which template did
	// this use, and what did it beat" is the question asked when the wrong
	// one is used, and a client that has to publish something to find out is
	// a client with no answer.
	Template string   `json:"template"`
	Searched []string `json:"searched,omitempty"`

	// Path is where the preview is served and Checksum names the bytes. Both
	// are absent in validate mode, which writes nothing.
	Path     string `json:"path,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`

	// Valid and Error are the answer in validate mode.
	Valid bool           `json:"valid"`
	Error *templateFault `json:"error,omitempty"`
}

// templateFault is a template that would not parse, as validate mode reports
// it.
type templateFault struct {
	Template string `json:"template"`
	Line     int    `json:"line,omitempty"`
	Phase    string `json:"phase"`
	Message  string `json:"message"`
}

// previewDocument is POST /api/v1/documents/{uid}/preview.
func (h *Handler) previewDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req previewRequest
	// A body is optional: "preview this" with one output channel and no
	// validation is the ordinary request, and requiring "{}" for it would be
	// a rule with no reason behind it.
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}

	result, err := h.svc.Preview(r.Context(), identity, service.PreviewRequest{
		UID:        r.PathValue("uid"),
		ChannelUID: req.Channel,
		Validate:   req.Validate,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	out := previewResponse{
		UID:      result.View.Document.UID,
		Mode:     result.Mode,
		Channel:  result.Channel.UID,
		Name:     result.Channel.Name,
		Version:  result.View.Version.Number,
		Draft:    result.View.Version.CheckedInAt.IsZero(),
		Category: result.Category.Path,
		URI:      result.URI,
		URL:      result.URL,
		Template: result.Template,
		Searched: result.Searched,
		Valid:    result.Valid,
	}
	if result.Problem != nil {
		out.Error = &templateFault{
			Template: result.Problem.Template,
			Line:     result.Problem.Line,
			Phase:    result.Problem.Phase,
			Message:  result.Problem.Error(),
		}
	}
	if result.Entry.Name != "" {
		out.Path = result.Entry.Path()
		out.Checksum = result.Entry.Checksum
		out.Bytes = result.Entry.Bytes
	}

	// 200 rather than 201. A preview is content-addressed, so the same
	// document rendered twice against the same template is the same file at
	// the same address: "created" would be a lie the second time and the
	// client cannot tell which time it is.
	writeJSON(w, http.StatusOK, out)
}

// servePreview is GET /preview/{name}.
func (h *Handler) servePreview(w http.ResponseWriter, r *http.Request, _ domain.Identity) {
	name := r.PathValue("name")
	f, err := h.svc.PreviewOpen(name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", render.ContentType(name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// A preview is served from the same origin as the editorial UI, and its
	// body is HTML an editor wrote -- markup included, since a block field
	// holds markup and the renderer's "raw" exists to emit it. Sandboxing it
	// puts the page in an opaque origin: scripts still run, so the preview
	// looks like the page will, and they cannot read the session cookie of
	// the person previewing or make a credentialed request as them.
	//
	// allow-same-origin is deliberately absent. Adding it would undo the
	// whole header while leaving it looking careful, which is the same
	// failure mode as gating a Secure cookie on r.TLS (invariant 13).
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups allow-forms")

	// Content-addressed and therefore immutable, but private: it is a
	// preview of unpublished content and no shared cache should hold it.
	w.Header().Set("Cache-Control", "private, max-age=300")

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		// The status line has gone; there is nothing to tell the client. It
		// is still worth a line in the log, because a preview that truncates
		// looks to the person reading it like a template that stops early.
		h.log.Error("serving a preview", "name", name, "error", err)
	}
}
