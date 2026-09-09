// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
)

// PLAN.md M13 acceptance 4: every page renders with an empty database without
// panicking.
//
// "Empty" is a migrated database with one user in it, which is the emptiest
// database anybody can sign in to: "cmsdb init" then "cmsdb bootstrap admin",
// and nothing else. There is no site, no element type, no category, no output
// channel, no document, and no job. Every screen has to say so rather than
// dereference what is not there -- a UI that only works against a seeded
// database is a UI nobody can use on the day they install this.

// recorder collects the patterns Register hands a mux, so that the test walks
// the routes the server actually mounts rather than a list beside them.
type recorder struct {
	mux      *http.ServeMux
	patterns []string
}

func (r *recorder) Handle(pattern string, handler http.Handler) {
	r.patterns = append(r.patterns, pattern)
	r.mux.Handle(pattern, handler)
}

// routes returns every pattern this package registers.
func routes(t *testing.T) []string {
	t.Helper()
	rec := &recorder{mux: http.NewServeMux()}
	Register(rec, Deps{Environment: config.Production})
	if len(rec.patterns) == 0 {
		t.Fatal("Register registered nothing")
	}
	return rec.patterns
}

// fill substitutes a wildcard with something that will not be found, which is
// the answer this test wants: a page that renders "no such document" is a page
// that rendered.
func fill(pattern string) (method, path string) {
	method, rest, _ := strings.Cut(pattern, " ")
	rest = strings.ReplaceAll(rest, "{$}", "")
	rest = strings.ReplaceAll(rest, "{uid}", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	rest = strings.ReplaceAll(rest, "{slug}", "mine")
	rest = strings.ReplaceAll(rest, "{file}", "app.css")
	return method, rest
}

// TestEveryPageRendersWithAnEmptyDatabase is acceptance 4.
func TestEveryPageRendersWithAnEmptyDatabase(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	session := h.session(t, "admin@example.com")

	for _, pattern := range routes(t) {
		method, path := fill(pattern)
		t.Run(pattern, func(t *testing.T) {
			var code int
			switch method {
			case http.MethodGet:
				code = h.get(t, path, session).Code
			case http.MethodPost:
				code = h.post(t, path, session, url.Values{}).Code
			default:
				t.Fatalf("%s is neither a GET nor a POST; this test does not know how to drive it", pattern)
			}
			if code >= http.StatusInternalServerError {
				t.Errorf("%s %s = %d against an empty database", method, path, code)
			}
		})
	}
}

// TestEveryPageRendersSignedOut asserts the other half of the same thing: a
// browser with no session gets the login form or a refusal, never a panic and
// never a page.
func TestEveryPageRendersSignedOut(t *testing.T) {
	h := newHarness(t)

	for _, pattern := range routes(t) {
		method, path := fill(pattern)
		t.Run(pattern, func(t *testing.T) {
			var code int
			switch method {
			case http.MethodGet:
				code = h.get(t, path, nil).Code
			case http.MethodPost:
				code = h.post(t, path, nil, url.Values{}).Code
			}
			if code >= http.StatusInternalServerError {
				t.Errorf("%s %s = %d signed out", method, path, code)
			}
		})
	}
}

// TestStaticIsServedFromTheBinary asserts that the two files the layout asks
// for are there. A stylesheet that 404s is a UI nobody would call finished,
// and an htmx.min.js that is not embedded is a UI whose fragments never swap.
func TestStaticIsServedFromTheBinary(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ file, contentType string }{
		{"app.css", "text/css; charset=utf-8"},
		{"htmx.min.js", "text/javascript; charset=utf-8"},
		{"htmx-LICENSE.txt", "text/plain; charset=utf-8"},
	} {
		w := h.get(t, StaticPrefix+tc.file, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s%s = %d", StaticPrefix, tc.file, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.contentType {
			t.Errorf("GET %s%s served %q, want %q", StaticPrefix, tc.file, got, tc.contentType)
		}
		if w.Body.Len() == 0 {
			t.Errorf("GET %s%s served nothing", StaticPrefix, tc.file)
		}
	}
	if got := h.get(t, StaticPrefix+"nothing.css", nil).Code; got != http.StatusNotFound {
		t.Errorf("GET a file that is not embedded = %d, want 404", got)
	}
}

// TestUnknownPathIsNotTheDashboard is why the root pattern is "GET /{$}".
//
// A catch-all "GET /" would turn every mistyped URL into the dashboard, and
// every unregistered API path into an HTML page. The route table would still
// be right and the server would still be wrong.
func TestUnknownPathIsNotTheDashboard(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	session := h.session(t, "admin@example.com")

	if got := h.get(t, "/nothing-is-here", session).Code; got != http.StatusNotFound {
		t.Errorf("GET an unregistered path = %d, want 404", got)
	}
}
