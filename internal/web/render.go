// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/edge"
	"github.com/mdhender/bricolage/internal/reqctx"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// mustParse builds the fragment set and one template set per page.
//
// A page's set is the layout, every partial, and that page's own "content"
// definition. Building one set per page is what lets every page define a
// template of the same name; the alternative is a differently named "content"
// per page, which is a convention nobody can forget to follow only once.
//
// It panics on a bad template because the templates are embedded: there is no
// configuration that could produce a parse error, no operator action that
// could fix one, and a server that started with half a UI would be a server
// whose first editor found out.
func mustParse() (map[string]*template.Template, *template.Template) {
	base := template.New("layout").Funcs(funcs)
	base = template.Must(base.ParseFS(templateFS,
		"templates/layout.gohtml", "templates/partials/*.gohtml"))

	pages, err := fs.Glob(templateFS, "templates/pages/*.gohtml")
	if err != nil {
		panic(fmt.Sprintf("web: globbing pages: %v", err))
	}
	if len(pages) == 0 {
		panic("web: no page templates were embedded")
	}

	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		clone, err := base.Clone()
		if err != nil {
			panic(fmt.Sprintf("web: cloning the layout for %s: %v", p, err))
		}
		out[path.Base(p)] = template.Must(clone.ParseFS(templateFS, p))
	}
	return out, base
}

// Base is what every page carries: who is looking, what to call the page, and
// the two counts the navigation shows.
//
// It is embedded in each page's own struct rather than holding an "any" for
// the rest, so that a template naming a field the handler does not set is a
// failure at execution and not a silently empty page.
type Base struct {
	Title string

	// SignedIn is false only on the login form, which is the one page that
	// renders without a session.
	SignedIn bool
	User     domain.User

	// Unread is the caller's unread notification count, drawn in the
	// navigation. It is the inbox's own count and nobody else's.
	Unread int

	// Development says whether this server is running with the development
	// affordances registered (invariant 16). The layout says so in a banner:
	// a person looking at a screen that has a log-me-in route behind it
	// should be told, in the same way the startup banner tells the operator.
	Development bool

	// Notice is a sentence about what just happened, carried in a query
	// parameter across the redirect that follows a POST.
	Notice string
}

// base fills in what every page shares.
//
// The unread count is a query per page. It is worth one: a notification a
// person is never shown is the failure DESIGN.md 10 is about, and a badge that
// is only right on the inbox page is a badge nobody believes.
func (h *Handler) base(r *http.Request, identity domain.Identity, title string) Base {
	b := Base{
		Title:       title,
		SignedIn:    identity.User.ID != 0,
		User:        identity.User,
		Development: h.env.IsDevelopment(),
		Notice:      r.URL.Query().Get("notice"),
	}
	if b.SignedIn {
		if inbox, err := h.svc.Notifications(r.Context(), identity, true, 1); err == nil {
			b.Unread = inbox.Unread
		}
	}
	return b
}

// render executes one page.
//
// The page is rendered into a buffer first. A template that fails halfway
// through would otherwise have written a 200 and half a document, and the
// browser would show the half: buffering is what makes "a template that fails
// leaves nothing behind" true here, as it is in internal/render.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	set, ok := h.pages[name]
	if !ok {
		h.fail(w, r, fmt.Errorf("web: no template %q", name))
		return
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", data); err != nil {
		h.fail(w, r, fmt.Errorf("rendering %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// partial executes one fragment, for an HTMX swap.
//
// It runs from the same set every page's is cloned from, so the fragment a
// handler returns is the definition the whole page drew rather than a second
// copy of it.
func (h *Handler) partial(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	var buf bytes.Buffer
	if err := h.fragments.ExecuteTemplate(&buf, name, data); err != nil {
		h.fail(w, r, fmt.Errorf("rendering %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// errorPage is what fail renders.
type errorPage struct {
	Base

	Status int
	Kind   string
	Detail string

	// Guard names the workflow guard that refused, when one did. It is the
	// same word the JSON API's problem document carries, so that somebody
	// who has read one recognises the other.
	Guard string

	// Fields are the invalid ones, when the refusal named any.
	Fields []domain.FieldError

	// RequestID joins a generic production message to a log line
	// (DESIGN.md 14).
	RequestID string
}

// fail renders err as an HTML error page, with the status internal/edge maps
// it to.
//
// The status is the point. A forged POST for a transition the engine refuses
// answers 409 here exactly as it does in the JSON API (PLAN.md M13
// acceptance 2), because both transports ask the same service method and both
// map the answer through the same function. A UI that redirected back to the
// document with a message would be a UI that reported a refusal as a
// success.
//
// What reaches the browser follows DESIGN.md 14's rule, which is internal/api's
// rule: a 4xx is the caller's own mistake and says so in both environments; a
// 5xx says nothing in production beyond a request id, except the 503 that
// names the flag this server was not started with.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, kind, title := edge.StatusFor(err)

	p := errorPage{
		Base:      Base{Title: title, Development: h.env.IsDevelopment()},
		Status:    status,
		Kind:      kind,
		RequestID: reqctx.RequestID(r.Context()),
	}
	if identity, ok := reqctx.Identity(r.Context()); ok {
		p.SignedIn = identity.User.ID != 0
		p.User = identity.User
	}
	if g, ok := domain.GuardOf(err); ok {
		p.Guard = string(g)
	}
	if fields, ok := domain.FieldErrorsOf(err); ok {
		p.Fields = fields
	}

	switch {
	case status < http.StatusInternalServerError:
		p.Detail = err.Error()
	case errors.Is(err, domain.ErrUnavailable):
		p.Detail = err.Error()
	case h.env.IsDevelopment():
		p.Detail = err.Error()
	default:
		p.Detail = "the server could not handle this request"
	}

	if status >= http.StatusInternalServerError {
		h.log.Error("request failed",
			"status", status, "method", r.Method, "path", r.URL.Path,
			"request_id", p.RequestID, "error", err)
	} else {
		h.log.Log(r.Context(), slog.LevelDebug, "request refused",
			"status", status, "method", r.Method, "path", r.URL.Path,
			"request_id", p.RequestID, "error", err)
	}

	set, ok := h.pages["error.gohtml"]
	if !ok {
		http.Error(w, title, status)
		return
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", p); err != nil {
		// The error page itself failing is the one case with nowhere left to
		// go. Say the status in plain text rather than recursing.
		http.Error(w, title, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// unavailable answers a request that reached a route built without a service,
// which is "cmsd routes" and a wiring mistake and nothing else.
func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, fmt.Errorf("this server was built without a database: %w", domain.ErrUnavailable))
}

// serveStatic serves one file from the embedded tree.
//
// The pattern's wildcard is a single segment, so there is no path to traverse
// out of, and the tree is embedded, so there is no directory to be pointed at
// (invariant 19 is not even in play).
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	body, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}

// hx reports whether HTMX made this request, which is what decides between a
// fragment and a redirect.
//
// Both paths exist deliberately. Every screen works with JavaScript turned
// off -- a form posts, the server answers 303, the browser follows it -- and
// HTMX turns the same POST into a fragment swap. One handler, two renderings
// of the same answer; a UI that only worked one way would be a UI that could
// not be tested without a browser.
func hx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// redirect answers a form POST with a See Other, optionally carrying a notice
// for the page it lands on.
func redirect(w http.ResponseWriter, r *http.Request, to, notice string) {
	if notice != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// maxFormBytes bounds a form body. Nothing this UI accepts is large, and an
// unbounded parse is an easy denial of service.
const maxFormBytes = 1 << 20

// form parses a submitted form, refusing one that is too large or malformed.
func form(r *http.Request) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("form: %v: %w", err, domain.ErrInvalid)
	}
	return nil
}

// funcs are the template helpers. They are formatting and nothing else: a
// function here that reached the service would be business logic in a
// template, which is the shape this system was built to avoid.
var funcs = template.FuncMap{
	// stamp renders an instant, or nothing at all for the zero time.
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"day": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02")
	},
	// forInput renders an instant for <input type="datetime-local">.
	"forInput": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02T15:04")
	},
	"zero": func(t time.Time) bool { return t.IsZero() },
	// pretty indents a JSON document for reading. Content that will not parse
	// is shown as it is: a draft may hold anything until it is checked in.
	"pretty": func(s string) string {
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
			return s
		}
		return buf.String()
	},
	// keys lists a map's keys in a stable order, so that a payload renders
	// the same way twice.
	"keys": func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	},
	"value": func(m map[string]any, k string) any { return m[k] },
	// dict builds a map for a partial that needs more than one value.
	"dict": func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf("dict: odd number of arguments")
		}
		out := make(map[string]any, len(pairs)/2)
		for i := 0; i < len(pairs); i += 2 {
			k, ok := pairs[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict: key %d is not a string", i)
			}
			out[k] = pairs[i+1]
		}
		return out, nil
	},
	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
}
