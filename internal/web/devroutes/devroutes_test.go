// Copyright (c) 2026 Michael D Henderson.

package devroutes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordingMux stands in for the route builder.
type recordingMux struct{ patterns []string }

func (m *recordingMux) Handle(pattern string, _ http.Handler) {
	m.patterns = append(m.patterns, pattern)
}

// TestRegisterAcceptsAServeMux is the property that keeps the Mux interface
// honest: it exists so the route table can be recorded, not so this package
// can invent a shape only it satisfies.
func TestRegisterAcceptsAServeMux(t *testing.T) {
	var _ Mux = http.NewServeMux()
	var _ Mux = &recordingMux{}
}

// TestRegisterRegistersBothMethods records the deliberate trade in
// DESIGN.md 11: GET is a footgun in general, but nothing links to this route
// and "curl" without "-X POST" is what an agent reaches for.
func TestRegisterRegistersBothMethods(t *testing.T) {
	m := &recordingMux{}
	Register(m, Deps{})

	want := map[string]bool{
		"GET " + Prefix + "shut-it-down":  false,
		"POST " + Prefix + "shut-it-down": false,
	}
	for _, p := range m.patterns {
		if _, ok := want[p]; !ok {
			t.Errorf("registered an unexpected pattern %q", p)
			continue
		}
		want[p] = true
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("pattern %q was not registered", p)
		}
	}
	for _, p := range m.patterns {
		if !strings.Contains(p, Prefix) {
			t.Errorf("pattern %q does not carry the %q prefix; the route table marks development routes by it", p, Prefix)
		}
	}
}

// TestShutdownIsRequestedAfterTheBodyIsWritten is the flush-before-shutdown
// requirement seen from inside: by the time the handler returns, the body is
// already in the response.
func TestShutdownIsRequestedAfterTheBodyIsWritten(t *testing.T) {
	reasons := make(chan string, 1)
	mux := http.NewServeMux()
	Register(mux, Deps{Shutdown: func(reason string) { reasons <- reason }})

	req := httptest.NewRequest(http.MethodGet, Prefix+"shut-it-down", nil)
	req.RemoteAddr = "127.0.0.1:11111"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "shutting down" {
		t.Fatalf("body = %q, want %q", got, "shutting down")
	}
	if got := <-reasons; got != ReasonShutdownRoute {
		t.Errorf("shutdown reason = %q, want %q", got, ReasonShutdownRoute)
	}
}

// TestPeerGuard is the second layer of the guard stack. The check must use the
// real TCP peer and never X-Forwarded-For, which is attacker-controlled unless
// the connection came from a trusted proxy — and distrusting the other end is
// the entire point of this check.
func TestPeerGuard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		forwarded  string
		wantStatus int
		wantCall   bool
	}{
		{name: "ipv4 loopback", remoteAddr: "127.0.0.1:5000", wantStatus: http.StatusOK, wantCall: true},
		{name: "ipv6 loopback", remoteAddr: "[::1]:5000", wantStatus: http.StatusOK, wantCall: true},
		{name: "127.0.0.2 is still loopback", remoteAddr: "127.0.0.2:5000", wantStatus: http.StatusOK, wantCall: true},
		{name: "public peer", remoteAddr: "203.0.113.7:5000", wantStatus: http.StatusNotFound},
		{name: "private peer", remoteAddr: "10.0.0.4:5000", wantStatus: http.StatusNotFound},
		{
			name:       "public peer claiming to be loopback",
			remoteAddr: "203.0.113.7:5000",
			forwarded:  "127.0.0.1",
			wantStatus: http.StatusNotFound,
		},
		{name: "unparseable peer fails closed", remoteAddr: "not-an-address", wantStatus: http.StatusNotFound},
		{name: "empty peer fails closed", remoteAddr: "", wantStatus: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := make(chan string, 1)
			mux := http.NewServeMux()
			Register(mux, Deps{Shutdown: func(reason string) { called <- reason }})

			req := httptest.NewRequest(http.MethodGet, Prefix+"shut-it-down", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if tc.wantCall {
				// The handler triggers the shutdown from a goroutine after
				// flushing, so an accepted request has to be waited for.
				select {
				case <-called:
				case <-time.After(5 * time.Second):
					t.Error("shutdown was never requested")
				}
				return
			}
			// A refused request never reached the handler, so no goroutine was
			// ever started and there is nothing to wait for.
			select {
			case reason := <-called:
				t.Errorf("a refused request requested shutdown anyway: %q", reason)
			default:
			}
		})
	}
}

// TestRegisterToleratesNilDeps keeps the package usable from a test that only
// wants the route table, without making a nil logger a panic in production.
func TestRegisterToleratesNilDeps(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Deps{})

	req := httptest.NewRequest(http.MethodGet, Prefix+"shut-it-down", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
