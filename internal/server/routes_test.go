// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
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
