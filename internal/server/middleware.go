// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/api"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// The middleware that wraps the whole mux. It lives here because it wraps
// every transport, and internal/server is the one place that knows about all
// of them.

// withRequestID gives every request an id, puts it on the context, and returns
// it in a header (DESIGN.md 14).
//
// A client-supplied X-Request-Id is not trusted or reused: an id that a caller
// chooses is an id a caller can make collide, and the whole value of this
// field is that it identifies one request in one log.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
	})
}

// newRequestID returns 12 random bytes, base64url. It is not a ULID: a request
// id needs to be unique in a log, not sortable, and nothing joins on it.
func newRequestID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		// There is nothing useful to do with this failure, and a request
		// without an id is better than a request that fails.
		return "unknown"
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// withClientAddr resolves the real client address once and puts it on the
// context (invariant 14, DESIGN.md 11).
//
// X-Forwarded-For is honoured only when the connection's peer is in the
// configured trusted-proxy list. Anywhere else the header is
// attacker-controlled, and the resolved address is what lands in the event log
// and, later, in the rate limiter -- so trusting it from an arbitrary peer
// means letting a caller choose what the audit log says about them.
//
// The resolution happens here and nowhere else. A second parse downstream is a
// second policy, and the two disagree in exactly one direction.
func withClientAddr(trusted []*net.IPNet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := resolveClientAddr(r, trusted)
		next.ServeHTTP(w, r.WithContext(reqctx.WithClientAddr(r.Context(), addr)))
	})
}

// resolveClientAddr returns the address to attribute this request to.
func resolveClientAddr(r *http.Request, trusted []*net.IPNet) string {
	peer := peerHost(r.RemoteAddr)
	if !isTrusted(peer, trusted) {
		return peer
	}

	// The proxy appends the address it saw, so the client is the last entry
	// this side of a trusted hop. Walking right to left and stopping at the
	// first untrusted entry is the standard reading: everything to the right
	// is a proxy we trust, and the first thing that is not is the furthest
	// address we have any reason to believe.
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer
	}
	var hops []string
	for _, v := range forwarded {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		candidate := peerHost(hops[i])
		if net.ParseIP(candidate) == nil {
			// An unparseable entry means the chain cannot be trusted past
			// this point. Stop rather than guessing.
			return peer
		}
		if !isTrusted(candidate, trusted) {
			return candidate
		}
	}
	// Every hop was a trusted proxy, so the peer is as far as this goes.
	return peer
}

// peerHost strips the port from an address, tolerating one that has none.
func peerHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

// isTrusted reports whether host is in the trusted-proxy list. An unparseable
// address is never trusted: failing closed is the only safe direction for a
// check whose failure mode is a forged audit log.
func isTrusted(host string, trusted []*net.IPNet) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// withCSRF applies net/http.CrossOriginProtection to everything that is not
// authenticated by a bearer token (DESIGN.md 11).
//
// The protection reads Sec-Fetch-Site and Origin, so it defends anything a
// browser can be made to send: the HTML UI, and any API route reached with the
// session cookie. A request carrying a bearer token is exempt, because a
// browser does not attach one on its own -- there is no ambient credential to
// abuse, which is the entire mechanism CSRF depends on.
//
// The public origin is registered as trusted so that the UI, served from it
// through the proxy, is same-origin as far as this is concerned.
func withCSRF(origin config.PublicOrigin, next http.Handler) (http.Handler, error) {
	protection := http.NewCrossOriginProtection()
	if s := origin.String(); s != "" {
		if err := protection.AddTrustedOrigin(s); err != nil {
			return nil, err
		}
	}
	protected := protection.Handler(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := api.BearerToken(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	}), nil
}
