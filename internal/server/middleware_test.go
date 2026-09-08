// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// TestClientAddrFromATrustedProxy is PLAN.md M2 acceptance 5, second half:
// X-Forwarded-For from 127.0.0.1 is honoured.
//
// TestClientAddrFromAnUntrustedPeer is the first half, and it is the one that
// matters: the header is attacker-controlled from anywhere else, and the
// resolved address is what lands in the event log and, later, in the rate
// limiter. Trusting it from an arbitrary peer lets a caller choose what the
// audit log says about them.
func TestClientAddr(t *testing.T) {
	trusted, err := config.ParseTrustedProxies(config.DefaultTrustedProxies)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{
			name: "no header, the peer is the client",
			peer: "203.0.113.5:4444",
			want: "203.0.113.5",
		},
		{
			name:      "a trusted proxy is believed",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"203.0.113.5"},
			want:      "203.0.113.5",
		},
		{
			name:      "an untrusted peer is not believed",
			peer:      "203.0.113.5:4444",
			forwarded: []string{"192.0.2.1"},
			want:      "203.0.113.5",
		},
		{
			name:      "an untrusted peer claiming to be loopback is not believed",
			peer:      "203.0.113.5:4444",
			forwarded: []string{"127.0.0.1"},
			want:      "203.0.113.5",
		},
		{
			name:      "the IPv6 loopback is trusted too",
			peer:      "[::1]:52000",
			forwarded: []string{"203.0.113.5"},
			want:      "203.0.113.5",
		},
		{
			name:      "a chain: the client is the last entry this side of a trusted hop",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"198.51.100.9, 203.0.113.5"},
			want:      "203.0.113.5",
		},
		{
			name:      "a chain a client forged the left of",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"1.2.3.4", "203.0.113.5"},
			want:      "203.0.113.5",
		},
		{
			name:      "a trusted proxy behind a trusted proxy",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"203.0.113.5, 127.0.0.1"},
			want:      "203.0.113.5",
		},
		{
			name:      "an unparseable entry stops the walk rather than guessing",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"not-an-address"},
			want:      "127.0.0.1",
		},
		{
			name:      "every hop trusted leaves the peer",
			peer:      "127.0.0.1:52000",
			forwarded: []string{"::1, 127.0.0.1"},
			want:      "127.0.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := withClientAddr(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = reqctx.ClientAddr(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = tc.peer
			for _, v := range tc.forwarded {
				req.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Errorf("client address = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestIDIsOursAndNotTheCallers: an id a caller chooses is an id a
// caller can make collide, and the whole value of the field is that it
// identifies one request in one log.
func TestRequestIDIsOursAndNotTheCallers(t *testing.T) {
	var seen string
	h := withRequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = reqctx.RequestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "chosen-by-the-caller")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if seen == "" {
		t.Fatal("no request id on the context")
	}
	if seen == "chosen-by-the-caller" {
		t.Error("the caller's request id was adopted")
	}
	if got := w.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("the response header says %q and the context says %q", got, seen)
	}

	// And it differs between requests, or it identifies nothing.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if seen == reqctx.RequestID(req.Context()) {
		t.Error("two requests got the same id")
	}
}

// TestCSRF is DESIGN.md 11: the protection covers anything a browser can be
// made to send, and a request carrying a bearer token is exempt because a
// browser does not attach one on its own.
func TestCSRF(t *testing.T) {
	origin, err := config.ParsePublicOrigin(config.DefaultPublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h, err := withCSRF(origin, next)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{
			name:   "a same-origin write is allowed",
			method: http.MethodPost,
			headers: map[string]string{
				"Sec-Fetch-Site": "same-origin",
			},
			want: http.StatusOK,
		},
		{
			name:   "a cross-site write is refused",
			method: http.MethodPost,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "a cross-site write carrying a bearer token is allowed",
			method: http.MethodPost,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Authorization":  "Bearer a-token",
			},
			want: http.StatusOK,
		},
		{
			name:   "a cross-site read is allowed; safe methods change nothing",
			method: http.MethodGet,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
			},
			want: http.StatusOK,
		},
		{
			name:   "a write from the public origin is allowed",
			method: http.MethodPost,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Origin":         config.DefaultPublicOrigin,
			},
			want: http.StatusOK,
		},
		{
			name:   "a write from somewhere else is refused",
			method: http.MethodPost,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Origin":         "https://evil.example.com",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "a non-browser write, with neither header, is allowed",
			method: http.MethodPost,
			want:   http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/sessions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
