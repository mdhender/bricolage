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
// Permitted imports: the standard library, and internal/domain for its
// sentinel errors. Nothing that can perform I/O, open a database, read a flag,
// or resolve the environment -- everything else is handed in through Deps.
package devroutes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
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
//
// Everything here is handed in. This package resolves nothing for itself --
// not the environment, not the origin, not a database -- because a development
// affordance that can reach out and get what it needs is a development
// affordance that can be wired up by accident.
type Deps struct {
	// Logger receives one WARN line per request reaching any handler here.
	Logger *slog.Logger

	// Shutdown begins a graceful shutdown. It must not block: the handler
	// calls it after flushing its response, and the shutdown it starts waits
	// for that handler to return.
	Shutdown func(reason string)

	// DevLogin issues a session for an existing user without a password. A
	// nil DevLogin leaves the log-me-in route unregistered, which is what a
	// server with no database does.
	//
	// It returns domain.ErrNotFound for an unknown email, and the handler
	// turns that into a 404: this route does not create accounts.
	DevLogin func(ctx context.Context, email, peer string) (Login, error)

	// ValidateReturnTo checks the returnTo parameter against the configured
	// public origin and returns the URL to redirect to. An error is a 400.
	//
	// The rule lives in internal/config, with the origin it is checked
	// against, rather than here: an open redirect in a development-only route
	// is still an open redirect, and this is the pattern that gets copied into
	// the route that is not development-only.
	ValidateReturnTo func(returnTo string) (string, error)

	// SetSessionCookie writes the session cookie for a token, with the
	// attributes invariant 13 requires. It is handed in so that there is one
	// cookie-writing path in the process rather than two.
	SetSessionCookie func(w http.ResponseWriter, token string, expires time.Time)
}

// Login is what DevLogin returns: the token, when it expires, and enough about
// the user to render a response.
type Login struct {
	Token     string
	ExpiresAt time.Time
	UserUID   string
	Email     string
	Name      string
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

	if deps.DevLogin != nil {
		mux.Handle("GET "+Prefix+"log-me-in/{email}", guard(log, "log-me-in", logMeIn(log, deps)))
	}
}

// logMeIn is GET /__development/log-me-in/{email}?returnTo={url}
// (DESIGN.md 11).
//
// It creates a session for an existing user without a password. It does not
// create accounts: an unknown email is a 404, which keeps the blast radius to
// accounts that already exist and matches what an agent actually needs --
// "cmsdb bootstrap admin", then log in as that admin.
//
// The session it issues is an ordinary session with the ordinary expiry. It
// grants no extra privilege: the user's roles and grants apply exactly as they
// would after a real login (PLAN.md M2 acceptance 10).
//
// The response depends on the request, because three different callers use it:
// a browser following a link, an agent asking for JSON, and a person with
// curl.
//
//	returnTo present            302 to it, session cookie set
//	Accept: application/json    200 with the token and the user
//	otherwise                   200 with the token as plain text
func logMeIn(log *slog.Logger, deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := r.PathValue("email")
		if email == "" {
			http.Error(w, "no email in the path", http.StatusBadRequest)
			return
		}

		// returnTo is validated before anything is created. An open redirect
		// is a refusal, not a session that also redirects somewhere bad.
		var redirect string
		if raw := r.URL.Query().Get("returnTo"); raw != "" {
			if deps.ValidateReturnTo == nil {
				http.Error(w, "returnTo is not configured on this server", http.StatusBadRequest)
				return
			}
			target, err := deps.ValidateReturnTo(raw)
			if err != nil {
				log.Warn("development route refused a returnTo",
					"route", "log-me-in", "returnTo", raw, "error", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			redirect = target
		}

		peer := peerAddr(r)
		login, err := deps.DevLogin(r.Context(), email, peer)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				log.Warn("development login refused: no such user",
					"route", "log-me-in", "email", email, "peer", peer)
				http.Error(w, fmt.Sprintf("no such user: %s", email), http.StatusNotFound)
				return
			}
			log.Error("development login failed",
				"route", "log-me-in", "email", email, "peer", peer, "error", err)
			http.Error(w, "could not log in", http.StatusInternalServerError)
			return
		}

		log.Warn("development login issued a session",
			"route", "log-me-in",
			"email", login.Email,
			"user", login.UserUID,
			"peer", peer,
			"expires_at", login.ExpiresAt.Format(time.RFC3339),
		)

		if deps.SetSessionCookie != nil {
			deps.SetSessionCookie(w, login.Token, login.ExpiresAt)
		}

		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}

		if wantsJSON(r) {
			body, err := json.Marshal(map[string]any{
				"token":      login.Token,
				"expires_at": login.ExpiresAt,
				"user": map[string]any{
					"uid":   login.UserUID,
					"email": login.Email,
					"name":  login.Name,
				},
			})
			if err != nil {
				http.Error(w, "could not encode the response", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, login.Token)
	}
}

// wantsJSON reports whether the caller asked for JSON.
//
// It is a substring test rather than a full Accept negotiation: a browser
// sends "text/html,application/xhtml+xml,...,*/*", which contains neither
// "application/json" nor anything this needs to weigh, and an agent sends
// exactly "application/json". Anything more is machinery for a distinction
// nothing here makes.
func wantsJSON(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(v), "application/json") {
			return true
		}
	}
	return false
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
