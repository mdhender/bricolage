// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/web/devroutes"
)

func newTestServer(t *testing.T, env config.Environment) *Server {
	t.Helper()
	// DeclareRoutesWithoutService is what "cmsd routes" passes: the table
	// carries every pattern serve would mount, with handlers that refuse
	// rather than dereference a service that is not there. Without it these
	// tests would assert against half a server.
	s, err := New(Options{
		Environment:                 env,
		Addr:                        "127.0.0.1:0",
		DeclareRoutesWithoutService: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestDevRoutesAbsentByDefault is PLAN.md M0 acceptance 8, and it gates
// release. With no --env and no CMS_ENV the environment resolves to
// production, and every /__development/* path must 404 because nothing is
// registered to answer it.
func TestDevRoutesAbsentByDefault(t *testing.T) {
	// Resolve from empty inputs, the same way cmsd does with no flag and no
	// variable set, rather than naming Production directly. The thing under
	// test is the whole chain, not the constant.
	res := config.Resolve(config.Inputs{})
	if res.Environment != config.Production {
		t.Fatalf("Resolve({}) = %v, want production", res.Environment)
	}

	s := newTestServer(t, res.Environment)
	for _, path := range []string{
		devroutes.Prefix + "shut-it-down",
		devroutes.Prefix + "log-me-in/anyone@example.com",
		devroutes.Prefix,
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
		}
	}
}

// TestRouteTableTellsTheTruth is PLAN.md M0 acceptance 9. The table cmsd
// routes prints is produced by the same call that registers the handlers, so
// it cannot claim a route the mux lacks or omit one the mux has.
func TestRouteTableTellsTheTruth(t *testing.T) {
	prod := newTestServer(t, config.Production)
	for _, r := range prod.Routes() {
		if r.IsDevelopment() {
			t.Errorf("production route table lists %q", r.Pattern)
		}
	}

	dev := newTestServer(t, config.Development)
	var found []string
	for _, r := range dev.Routes() {
		if r.IsDevelopment() {
			found = append(found, r.Pattern)
		}
	}
	if len(found) == 0 {
		t.Fatal("development route table lists no /__development/* pattern; " +
			"the production assertion above would pass even if the feature broke")
	}

	// Every pattern the table claims must actually answer, and every
	// development pattern must be absent from the production mux.
	for _, r := range dev.Routes() {
		method, path, ok := strings.Cut(r.Pattern, " ")
		if !ok {
			t.Errorf("pattern %q has no method; ServeMux patterns here are method-aware", r.Pattern)
			continue
		}
		// Two wildcards have to be filled in rather than sent literally.
		// "{$}" is an end-of-path marker and not a segment, and "{file}" is
		// matched by a handler that answers 404 for a file it does not have --
		// which is a handler answering, not the mux failing to route. Every
		// other wildcard is happy to match its own name.
		path = strings.ReplaceAll(path, "{$}", "")
		path = strings.ReplaceAll(path, "{file}", "app.css")

		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		dev.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("development mux answers %q with 404 but the table lists it", r.Pattern)
		}
	}
}

// TestHealthz covers PLAN.md M0 acceptance 5 at the handler level; the
// end-to-end half runs through the Caddy service and is not a unit test.
func TestHealthz(t *testing.T) {
	s := newTestServer(t, config.Production)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// TestPreviewMountIsInTheTable is PLAN.md M8: previews are served under
// /preview/, and "cmsd routes" is how an operator asks what this server
// mounts. It is neither a JSON API route nor a development affordance, and the
// table classifies it as itself.
func TestPreviewMountIsInTheTable(t *testing.T) {
	for _, env := range []config.Environment{config.Production, config.Development} {
		s := newTestServer(t, env)

		var found []Route
		for _, r := range s.Routes() {
			if r.IsPreview() {
				found = append(found, r)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%v: the table lists %d preview routes, want 1", env, len(found))
		}
		if found[0].IsAPI() || found[0].IsDevelopment() {
			t.Errorf("%v: the preview mount %q is classified as api=%v development=%v",
				env, found[0].Pattern, found[0].IsAPI(), found[0].IsDevelopment())
		}
		if !strings.HasPrefix(found[0].Pattern, "GET "+render.PreviewPrefix) {
			t.Errorf("%v: the preview mount is %q, want a GET under %q",
				env, found[0].Pattern, render.PreviewPrefix)
		}
	}
}

// TestUIPerformsNothingEarlCannot is PLAN.md M13 acceptance 5.
//
// Every write the HTML UI offers is one the JSON API offers too, so every
// screen is something earl can drive -- which is what makes the UI a second
// face on one application rather than a second application. A use case that
// existed only behind a form would be a use case that escaped the service
// layer and could not be scripted, tested from the command line, or exercised
// by the acceptance harness every milestone ships with.
//
// The table is checked in both directions. A UI route with no counterpart is
// the failure the acceptance criterion names; a UI route missing from the
// table is the way that failure would arrive unnoticed.
func TestUIPerformsNothingEarlCannot(t *testing.T) {
	// The UI spells with POST what the API spells with DELETE or PUT, because
	// an HTML form may only GET or POST. What has to match is the operation,
	// not the method.
	counterpart := map[string]string{
		"POST /login":                              "POST /api/v1/sessions",
		"POST /logout":                             "DELETE /api/v1/sessions/current",
		"POST /documents":                          "POST /api/v1/documents",
		"POST /documents/{uid}/edit":               "PATCH /api/v1/documents/{uid}",
		"POST /documents/{uid}/checkout":           "POST /api/v1/documents/{uid}/checkout",
		"POST /documents/{uid}/checkout/cancel":    "DELETE /api/v1/documents/{uid}/checkout",
		"POST /documents/{uid}/checkin":            "POST /api/v1/documents/{uid}/checkin",
		"POST /documents/{uid}/revert":             "POST /api/v1/documents/{uid}/revert",
		"POST /documents/{uid}/transitions":        "POST /api/v1/documents/{uid}/transitions",
		"POST /documents/{uid}/assignment":         "POST /api/v1/documents/{uid}/assignment",
		"POST /documents/{uid}/assignment/clear":   "DELETE /api/v1/documents/{uid}/assignment",
		"POST /documents/{uid}/due":                "PUT /api/v1/documents/{uid}/due",
		"POST /documents/{uid}/categories":         "PUT /api/v1/documents/{uid}/categories",
		"POST /documents/{uid}/comments":           "POST /api/v1/documents/{uid}/comments",
		"POST /comments/{uid}/resolution":          "POST /api/v1/comments/{uid}/resolution",
		"POST /documents/{uid}/approvals":          "POST /api/v1/documents/{uid}/approvals",
		"POST /documents/{uid}/approvals/withdraw": "DELETE /api/v1/documents/{uid}/approvals/current",
		"POST /documents/{uid}/preview":            "POST /api/v1/documents/{uid}/preview",
		"POST /documents/{uid}/publications":       "POST /api/v1/documents/{uid}/publications",
		"POST /notifications/{uid}/read":           "POST /api/v1/notifications/{uid}/read",
		"POST /jobs/{uid}/retry":                   "POST /api/v1/jobs/{uid}/retry",
		"POST /admin/grants":                       "POST /api/v1/grants",
		"POST /admin/roles":                        "POST /api/v1/users/{uid}/roles",

		// Invitations (issue #6). The revocation is the one operation whose
		// two spellings are identical, which is deliberate: no invitation row
		// is ever deleted, so a DELETE would be the one DELETE in the API that
		// does not delete, and the form could not spell it anyway.
		"POST /admin/invitations":              "POST /api/v1/invitations",
		"POST /admin/invitations/{uid}/revoke": "POST /api/v1/invitations/{uid}/revoke",
		"POST /invite":                         "POST /api/v1/invitations/redemption",
	}

	s := newTestServer(t, config.Production)
	declared := map[string]bool{}
	for _, r := range s.Routes() {
		declared[r.Pattern] = true
	}

	for _, r := range s.Routes() {
		if !r.IsUI() || !strings.HasPrefix(r.Pattern, "POST ") {
			continue
		}
		api, ok := counterpart[r.Pattern]
		if !ok {
			t.Errorf("%s writes something and this table does not say which API route does the same; either name it or the UI has grown an operation earl cannot perform (PLAN.md M13 acceptance 5)", r.Pattern)
			continue
		}
		if !declared[api] {
			t.Errorf("%s is answered by %s, which this server does not register", r.Pattern, api)
		}
	}

	// The other direction: a mapping left behind after its screen was removed
	// would quietly stop asserting anything.
	for pattern := range counterpart {
		if !declared[pattern] {
			t.Errorf("the table names %s, which the UI does not register", pattern)
		}
	}
}

// TestUIRoutesAreRegisteredBesideTheAPI asserts that the HTML UI is in the
// table "cmsd routes" prints, and that none of it landed under the API prefix
// or the development prefix.
func TestUIRoutesAreRegisteredBesideTheAPI(t *testing.T) {
	s := newTestServer(t, config.Production)

	var ui int
	for _, r := range s.Routes() {
		if !r.IsUI() {
			continue
		}
		ui++
		if r.IsAPI() || r.IsDevelopment() || r.IsPreview() {
			t.Errorf("%s is classified as more than one kind of route", r.Pattern)
		}
	}
	if ui == 0 {
		t.Fatal("the table carries no HTML UI routes")
	}
	if !slices.ContainsFunc(s.Routes(), func(r Route) bool { return r.Pattern == "GET /{$}" }) {
		t.Error("the dashboard is not registered at \"GET /{$}\"; a catch-all \"GET /\" would answer every mistyped path with a page")
	}
}
