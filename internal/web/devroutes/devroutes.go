// Copyright (c) 2026 Michael D Henderson.

// Package devroutes holds the /__development/* handlers: the concessions that
// let an automated agent drive the system without a human at a keyboard
// (DESIGN.md 11, "Development affordances").
//
// # No build tag gates this package
//
// An earlier draft of the design gated these routes on -tags dev as well as on
// the environment. That is gone, deliberately: such a tag means every local
// invocation has to go through "go build -tags dev" first, which breaks
// "go run ./cmd/cmsd", and a guard that makes the normal workflow impossible
// gets worked around. A worked-around guard protects nothing.
//
// So the resolved environment is the only switch, and the footgun that follows
// is accepted knowingly: anyone who exports CMS_ENV=development on a production
// server exposes a complete authentication bypass. What remains is three
// layers, and the last two exist precisely because the first can be
// misconfigured.
//
//  1. The route builder calls Register only when the resolved environment is
//     exactly "development" (invariant 16). This package does not check the
//     environment itself and must never learn how — a second switch meaning
//     almost the same thing is how both of them end up set in production.
//     The handlers are never added to the mux, so "cmsd routes" tells the
//     truth and there is no matched-then-refused path to get wrong.
//  2. Every handler refuses a non-loopback peer, using the real TCP peer and
//     never X-Forwarded-For. Behind the proxy the peer is always loopback, so
//     this does not help there; it exists to stop the routes answering a direct
//     connection from another machine.
//  3. Every request that reaches a handler here logs at WARN.
//
// Permitted imports: the standard library. It is handed what it needs.
package devroutes

import (
	"log/slog"
	"net"
	"net/http"
)

// Prefix is the path prefix every route in this package shares. The route
// builder and its tests use it to assert that nothing here reached a mux it
// should not have.
const Prefix = "/__development/"

// Mux is the part of *http.ServeMux that Register needs.
//
// It is an interface so that the route table "cmsd routes" prints can be
// produced by the same call that registers the handlers, rather than by a
// second list that can drift from it.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Deps are what the handlers need from the server that owns them.
type Deps struct {
	// Logger receives one WARN line per request reaching any handler here.
	Logger *slog.Logger

	// Shutdown begins a graceful shutdown. It must not block: the handler
	// calls it after flushing its response, and the shutdown it starts waits
	// for that handler to return.
	Shutdown func(reason string)
}

// ReasonShutdownRoute is the shutdown reason this package reports, so that the
// log line names which of the three paths into shutdown was taken.
const ReasonShutdownRoute = "development route"

// Register adds the development routes to mux.
//
// Call it only when the resolved environment is exactly "development". This
// function does not check, deliberately: the check belongs to the one place
// that owns the route table, where it can be seen and tested.
func Register(mux Mux, deps Deps) {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	shutdown := guard(log, "shut-it-down", shutItDown(deps.Shutdown))
	mux.Handle("GET "+Prefix+"shut-it-down", shutdown)
	mux.Handle("POST "+Prefix+"shut-it-down", shutdown)
}

// shutItDown writes its response and flushes it before beginning shutdown, so
// the caller gets a 200 rather than a connection reset (DESIGN.md 11). The
// shutdown is triggered from a goroutine after the flush.
//
// Both GET and POST are accepted. GET is a footgun in general — a link prefetch
// can trigger it — but nothing links to this route, and "curl" without
// "-X POST" is what an agent will reach for. Accepting both is the right trade
// here; do not copy the pattern elsewhere.
func shutItDown(shutdown func(reason string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("shutting down\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if shutdown != nil {
			go shutdown(ReasonShutdownRoute)
		}
	}
}

// guard wraps a development handler in the two layers that are this package's
// own: the WARN log line, and the loopback-peer refusal.
func guard(log *slog.Logger, name string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := peerAddr(r)
		log.Warn("development route",
			"route", name,
			"method", r.Method,
			"path", r.URL.Path,
			"peer", peer,
		)
		if !peerIsLoopback(peer) {
			log.Warn("development route refused: peer is not loopback",
				"route", name,
				"peer", peer,
			)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h(w, r)
	})
}

// peerAddr returns the host part of the real TCP peer address.
//
// It never consults X-Forwarded-For. That header is attacker-controlled unless
// the connection came from a configured trusted proxy (DESIGN.md 11), and the
// whole point of this check is to distrust the thing on the other end.
func peerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// peerIsLoopback reports whether host is a loopback literal.
//
// An unparseable or empty address is not loopback. Failing closed is the only
// safe direction for a check whose failure mode is an authentication bypass.
func peerIsLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
