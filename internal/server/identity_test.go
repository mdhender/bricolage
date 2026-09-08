// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

// TestResolvedClientAddressReachesTheEvent is invariant 14 through the whole
// stack, which is the only place it can be checked.
//
// The middleware resolves the address, puts it on the context, and the handler
// reads it from there; the service writes it into the event. Any one of those
// three tested alone proves nothing about the other two, and the failure this
// guards against -- an event log naming an address the caller chose -- is
// invisible until somebody is reading it during an incident.
func TestResolvedClientAddressReachesTheEvent(t *testing.T) {
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := clock.NewFake(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	svc, err := service.New(db, service.Options{Clock: c})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	if _, err := svc.CreateUser(t.Context(), service.NewUser{
		Email: email, Name: "Admin", Password: password,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	trusted, err := config.ParseTrustedProxies(config.DefaultTrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Environment:    config.Production,
		Addr:           "127.0.0.1:0",
		Service:        svc,
		TrustedProxies: trusted,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name      string
		peer      string
		forwarded string
		want      string
	}{
		{
			name:      "a trusted proxy is believed",
			peer:      "127.0.0.1:52000",
			forwarded: "203.0.113.5",
			want:      "203.0.113.5",
		},
		{
			name:      "an untrusted peer's header is ignored and the peer is used",
			peer:      "198.51.100.9:4444",
			forwarded: "203.0.113.5",
			want:      "198.51.100.9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"email":"` + email + `","password":"` + password + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			req.RemoteAddr = tc.peer

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201\n%s", w.Code, w.Body)
			}

			recorded, err := db.EventsOfType(t.Context(), events.SessionCreated, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(recorded) != 1 {
				t.Fatalf("read %d events, want 1", len(recorded))
			}
			if got := recorded[0].Payload["client"]; got != tc.want {
				t.Errorf("the event names client %v, want %q", got, tc.want)
			}

			// And the request id the middleware minted came back, so a log
			// line and a client's complaint can be joined up.
			if w.Header().Get("X-Request-Id") == "" {
				t.Error("no X-Request-Id on the response")
			}
		})
	}
}

// TestServeMountsTheMiddleware: Handler is what Serve serves, so a test that
// drives Handler drives what a client reaches. Reaching past the middleware is
// how a CSRF exemption or a forged X-Forwarded-For gets tested into existence.
func TestServeMountsTheMiddleware(t *testing.T) {
	s := newTestServer(t, config.Production)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("Handler() does not carry the request-id middleware")
	}

	// The bare mux does not, which is what makes the assertion above mean
	// something.
	w = httptest.NewRecorder()
	s.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Header().Get("X-Request-Id") != "" {
		t.Error("Mux() carries the middleware; the two accessors are the same thing")
	}
}
