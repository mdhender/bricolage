// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/web/devroutes"
)

// listen gives the test a real loopback listener on an arbitrary port, so the
// shutdown path runs against net/http rather than a recorder.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// serveInBackground runs s until it returns and hands back a channel carrying
// the result, so a test can assert that shutdown actually completed.
func serveInBackground(ctx context.Context, t *testing.T, s *Server, ln net.Listener) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { done <- s.Serve(ctx, ln) })
	t.Cleanup(wg.Wait)
	return done
}

func waitFor(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("Serve did not return within %s", within)
		return nil
	}
}

// TestTimeoutShutsDownCleanly is PLAN.md M0 acceptance 7. An expired timeout
// is a normal shutdown, not a failure, so Serve returns nil and cmsd exits 0.
//
// The timer is driven through the afterFunc seam rather than by waiting: the
// thing worth testing is that the timeout reaches the one shutdown path, and
// spending two seconds to learn that time passes is not a test. The real
// duration is exercised end to end in TestServeRealTimeout below.
func TestTimeoutShutsDownCleanly(t *testing.T) {
	s, err := New(Options{Environment: config.Production, Addr: "127.0.0.1:0", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fired := make(chan struct{})
	s.afterFunc = func(d time.Duration, f func()) *time.Timer {
		if d != time.Hour {
			t.Errorf("afterFunc got %v, want the configured timeout %v", d, time.Hour)
		}
		// Fire immediately; the point is the wiring, not the delay.
		go func() { f(); close(fired) }()
		return time.NewTimer(time.Hour)
	}

	done := serveInBackground(t.Context(), t, s, listen(t))
	<-fired
	if err := waitFor(t, done, 5*time.Second); err != nil {
		t.Fatalf("Serve = %v, want nil: an expired timeout is a normal shutdown", err)
	}
}

// TestServeRealTimeout is the same path with a real, short duration, because
// the seam above cannot prove that time.AfterFunc is wired to it at all.
func TestServeRealTimeout(t *testing.T) {
	s, err := New(Options{Environment: config.Production, Addr: "127.0.0.1:0", Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	done := serveInBackground(t.Context(), t, s, listen(t))
	if err := waitFor(t, done, 10*time.Second); err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("Serve returned after %v, before the %v timeout could have fired", elapsed, 150*time.Millisecond)
	}
}

// TestSignalShutsDownCleanly covers the third way into the one shutdown path:
// a cancelled context, which is how SIGTERM arrives from main.
func TestSignalShutsDownCleanly(t *testing.T) {
	s, err := New(Options{Environment: config.Production, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := serveInBackground(ctx, t, s, listen(t))
	cancel()

	if err := waitFor(t, done, 5*time.Second); err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
}

// TestDevShutdownFlushesBeforeExiting is PLAN.md M0 acceptance 10, and the
// assertion that matters is on the received body rather than on the exit code.
// The handler must write and flush its response before beginning shutdown, so
// the caller gets a 200 rather than a connection reset (DESIGN.md 11).
func TestDevShutdownFlushesBeforeExiting(t *testing.T) {
	s, err := New(Options{Environment: config.Development, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln := listen(t)
	done := serveInBackground(t.Context(), t, s, ln)

	url := "http://" + ln.Addr().String() + devroutes.Prefix + "shut-it-down"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if readErr != nil {
		t.Fatalf("reading the body failed with %v; the response was not flushed before shutdown began", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "shutting down" {
		t.Errorf("body = %q, want %q", got, "shutting down")
	}

	if err := waitFor(t, done, 5*time.Second); err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
}

// TestDevShutdownRefusesNonLoopbackPeer covers the second layer of the guard
// stack (DESIGN.md 11). Behind the proxy the peer is always loopback, so this
// does not help there; it exists to stop the route answering a direct
// connection from another machine.
func TestDevShutdownRefusesNonLoopbackPeer(t *testing.T) {
	s, err := New(Options{Environment: config.Development, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, devroutes.Prefix+"shut-it-down", nil)
	req.RemoteAddr = "203.0.113.7:40000"
	// The forwarded header is exactly the lie this check must not believe.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-loopback peer", rec.Code)
	}

	select {
	case reason := <-s.shutdown:
		t.Fatalf("a refused request requested shutdown anyway: %q", reason)
	default:
	}
}

// TestBannerPrintsEnvironmentInBothEnvironments is PLAN.md M0 acceptance 13.
// The banner is plain text in both environments on purpose: in production the
// structured log is JSON, and an operator greps for "environment=" without
// knowing the log format (DESIGN.md 11, 14).
func TestBannerPrintsEnvironmentInBothEnvironments(t *testing.T) {
	for _, tc := range []struct {
		env      config.Environment
		want     string
		wantWarn bool
	}{
		{env: config.Production, want: "environment=production", wantWarn: false},
		{env: config.Development, want: "environment=development", wantWarn: true},
	} {
		t.Run(tc.env.String(), func(t *testing.T) {
			var buf bytes.Buffer
			s, err := New(Options{Environment: tc.env, Addr: "127.0.0.1:0", Banner: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			s.WriteBanner()

			got := buf.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("banner = %q, want it to contain %q", got, tc.want)
			}
			hasWarn := strings.Contains(got, "*** ENVIRONMENT=development")
			if hasWarn != tc.wantWarn {
				t.Errorf("banner = %q, loud warning = %v, want %v", got, hasWarn, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(got, "can log in as any user") {
				t.Errorf("banner = %q, want the warning to say what the risk is", got)
			}
		})
	}
}

// TestBannerNamesTheSource is what makes the banner an incident signal rather
// than decoration: it says not just what the environment is but why.
func TestBannerNamesTheSource(t *testing.T) {
	res := config.Resolve(config.Inputs{Env: "development"})
	var buf bytes.Buffer
	s, err := New(Options{Environment: res.Environment, Addr: "127.0.0.1:0", Banner: &buf, Resolution: &res})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.WriteBanner()

	if got := buf.String(); !strings.Contains(got, "source=CMS_ENV") {
		t.Errorf("banner = %q, want it to name CMS_ENV as the source", got)
	}
}

// TestNewRejectsAnUnusableAddr keeps the failure at construction, where the
// operator sees it, rather than at listen time.
func TestNewRejectsAnUnusableAddr(t *testing.T) {
	if _, err := New(Options{Environment: config.Production, Addr: "no-port"}); err == nil {
		t.Error("New with a portless addr = nil, want an error")
	}
	if _, err := New(Options{Environment: config.Production, Timeout: -time.Second}); err == nil {
		t.Error("New with a negative timeout = nil, want an error")
	}
}

// TestNewDefaultsToTheLoopbackAddr pairs with the config test: the default has
// to survive the trip through Options too.
func TestNewDefaultsToTheLoopbackAddr(t *testing.T) {
	s, err := New(Options{Environment: config.Production})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() != config.DefaultAddr {
		t.Fatalf("Addr() = %q, want %q", s.Addr(), config.DefaultAddr)
	}
	if !config.AddrIsLoopback(s.Addr()) {
		t.Fatalf("Addr() = %q, which is not loopback", s.Addr())
	}
}
