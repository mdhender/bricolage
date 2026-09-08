// Copyright (c) 2026 Michael D Henderson.

package devroutes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
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

// The log-me-in tests. Everything the handler can do arrives through Deps, so
// these drive it with a stub rather than a database: what is under test is the
// route, the guard, the returnTo refusal, and the shape of the three
// responses (DESIGN.md 11).

// stubLogin returns a DevLogin that answers for one email and reports
// domain.ErrNotFound for anything else.
func stubLogin(known string, seen *string) func(context.Context, string, string) (Login, error) {
	return func(_ context.Context, email, peer string) (Login, error) {
		if seen != nil {
			*seen = peer
		}
		if email != known {
			return Login{}, fmt.Errorf("no such user %q: %w", email, domain.ErrNotFound)
		}
		return Login{
			Token:     "a-token",
			ExpiresAt: time.Date(2026, 2, 3, 16, 5, 6, 0, time.UTC),
			UserUID:   "01abcdefghijklmnopqrstuvwx",
			Email:     email,
			Name:      "Admin",
		}, nil
	}
}

// devMux builds a mux with the development routes and a working log-me-in.
func devMux(seen *string) *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux, Deps{
		DevLogin:         stubLogin("admin@example.com", seen),
		ValidateReturnTo: func(s string) (string, error) { return validReturnTo(s) },
		SetSessionCookie: func(w http.ResponseWriter, token string, expires time.Time) {
			http.SetCookie(w, &http.Cookie{
				Name: "__Host-cms_session", Value: token, Path: "/",
				Expires: expires, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
		},
	})
	return mux
}

// validReturnTo stands in for config.PublicOrigin.ValidateReturnTo, which is
// tested where it lives. This package only has to refuse what it is told to
// refuse.
func validReturnTo(s string) (string, error) {
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return s, nil
	}
	return "", fmt.Errorf("returnTo %q: not this origin", s)
}

func devRequest(path string, accept string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:5000"
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req
}

// TestLogMeInIsNotRegisteredWithoutAService: a server with no database has the
// shutdown route and nothing else, and the route table says so.
func TestLogMeInIsNotRegisteredWithoutAService(t *testing.T) {
	m := &recordingMux{}
	Register(m, Deps{})
	for _, p := range m.patterns {
		if strings.Contains(p, "log-me-in") {
			t.Errorf("registered %q with no DevLogin behind it", p)
		}
	}
}

// TestLogMeInJSON is the response an agent gets.
func TestLogMeInJSON(t *testing.T) {
	var peer string
	mux := devMux(&peer)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, devRequest(Prefix+"log-me-in/admin@example.com", "application/json"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body)
	}
	var body struct {
		Token string `json:"token"`
		User  struct {
			UID   string `json:"uid"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, rec.Body)
	}
	if body.Token != "a-token" || body.User.Email != "admin@example.com" {
		t.Errorf("body = %+v", body)
	}

	// The peer reaches the service, because the event it writes carries it
	// (PLAN.md M2 acceptance 13).
	if peer != "127.0.0.1" {
		t.Errorf("the handler passed peer %q, want the real TCP peer", peer)
	}

	// It is an ordinary session, so it gets the ordinary cookie: Secure even
	// though this connection has no TLS (invariant 13).
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1", len(cookies))
	}
	if !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Errorf("cookie = %+v, want Secure and HttpOnly", cookies[0])
	}
}

// TestLogMeInPlainText is what somebody with curl and no Accept header gets.
func TestLogMeInPlainText(t *testing.T) {
	mux := devMux(nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, devRequest(Prefix+"log-me-in/admin@example.com", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "a-token" {
		t.Errorf("body = %q, want the token", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// TestLogMeInRedirects is the browser case: 302 to returnTo, with the cookie
// set.
func TestLogMeInRedirects(t *testing.T) {
	mux := devMux(nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, devRequest(Prefix+"log-me-in/admin@example.com?returnTo=/documents", "text/html"))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302\n%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != "/documents" {
		t.Errorf("Location = %q, want \"/documents\"", got)
	}
	if len(rec.Result().Cookies()) != 1 {
		t.Error("the redirect set no session cookie")
	}
}

// TestLogMeInRefusesABadReturnTo: an open redirect is a refusal, and no
// session is created on the way to it. Validating before creating is what
// makes that true.
func TestLogMeInRefusesABadReturnTo(t *testing.T) {
	var peer string
	mux := devMux(&peer)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, devRequest(Prefix+"log-me-in/admin@example.com?returnTo=//evil.example.com", ""))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body)
	}
	if peer != "" {
		t.Error("a refused returnTo still reached the login")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a refused returnTo still set a cookie")
	}
}

// TestLogMeInUnknownEmail is PLAN.md M2 acceptance 11 at the handler: a 404,
// and no account. This route does not create accounts, which is what keeps the
// blast radius to accounts that already exist.
func TestLogMeInUnknownEmail(t *testing.T) {
	mux := devMux(nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, devRequest(Prefix+"log-me-in/nobody@example.com", "application/json"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a 404 set a session cookie")
	}
}

// TestLogMeInRefusesANonLoopbackPeer is the second layer of the guard stack,
// for this route as well as for shut-it-down. Behind the proxy the peer is
// always loopback, so it does not help there; it stops the route answering a
// direct connection from another machine.
func TestLogMeInRefusesANonLoopbackPeer(t *testing.T) {
	var peer string
	mux := devMux(&peer)

	req := devRequest(Prefix+"log-me-in/admin@example.com", "application/json")
	req.RemoteAddr = "203.0.113.7:5000"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if peer != "" {
		t.Error("a refused peer still reached the login")
	}
}
