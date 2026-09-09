// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"log/slog"
	"net/http"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/service"
)

// Prefix is the path every route in this package shares. It is a constant so
// that the route table and its tests refer to one string.
const Prefix = "/api/v1/"

// Mux is the part of *http.ServeMux that Register needs.
//
// It is an interface for the same reason devroutes.Mux is: the route table
// "cmsd routes" prints is produced by the same call that registers the
// handlers, so it cannot claim a route the mux does not have.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Deps are what the handlers need from the server that owns them.
type Deps struct {
	// Service is the use-case layer. Handlers parse a request, call one
	// method on it, and render the result; they hold no business logic and
	// never reach the store (DESIGN.md 3).
	Service *service.Service

	// Environment decides how much of an error reaches the client
	// (DESIGN.md 14). It gates no route here: the /__development/* routes are
	// gated at registration, in internal/server, and this package registers
	// none of them.
	Environment config.Environment

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger
}

// Handler holds the dependencies the routes share.
type Handler struct {
	svc *service.Service
	env config.Environment
	log *slog.Logger
}

// New builds the handler set.
func New(deps Deps) *Handler {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		svc: deps.Service,
		env: deps.Environment,
		log: log,
	}
}

// Register adds the JSON API routes to mux (DESIGN.md 12).
//
// M2 registered the session and identity routes and the grant-writing route
// that carries the anti-escalation check; M3 adds the document, version, diff,
// and history routes. The workflow and publishing routes arrive with the
// milestones that implement them; a route registered before its service method
// exists is a 501 nobody asked for.
func Register(mux Mux, deps Deps) {
	h := New(deps)

	// With no service there is no database, and the routes are registered
	// anyway, answering 503.
	//
	// That is for "cmsd routes", whose job is to print the table "serve" would
	// mount (DESIGN.md 11). A table that left the API out because no database
	// was open would answer the question it is asked -- "are the
	// /__development/* routes registered" -- against a table that is not the
	// one serve builds. Registering a handler that refuses keeps the table
	// honest without giving anything a nil service to dereference.
	handle := func(pattern string, handler http.Handler) {
		if deps.Service == nil {
			handler = http.HandlerFunc(unavailable)
		}
		mux.Handle(pattern, handler)
	}

	handle("POST "+Prefix+"sessions", http.HandlerFunc(h.createSession))
	handle("DELETE "+Prefix+"sessions/current", h.authenticated(h.deleteCurrentSession))
	handle("GET "+Prefix+"me", h.authenticated(h.me))
	handle("POST "+Prefix+"grants", h.authenticated(h.createGrant))
	handle("POST "+Prefix+"users/{uid}/roles", h.authenticated(h.assignRole))

	// Documents, versions, and history (PLAN.md M3), and the transitions
	// M4 adds. Transitions are a subresource on purpose (DESIGN.md 12): GET
	// says what the state machine permits and why, POST performs one. There
	// is deliberately no route here that sets a state directly.
	handle("GET "+Prefix+"documents", h.authenticated(h.listDocuments))
	handle("POST "+Prefix+"documents", h.authenticated(h.createDocument))
	handle("GET "+Prefix+"documents/{uid}", h.authenticated(h.showDocument))
	handle("PATCH "+Prefix+"documents/{uid}", h.authenticated(h.patchDocument))
	handle("POST "+Prefix+"documents/{uid}/checkout", h.authenticated(h.checkoutDocument))
	handle("DELETE "+Prefix+"documents/{uid}/checkout", h.authenticated(h.cancelCheckout))
	handle("POST "+Prefix+"documents/{uid}/checkin", h.authenticated(h.checkinDocument))
	handle("POST "+Prefix+"documents/{uid}/revert", h.authenticated(h.revertDocument))
	handle("GET "+Prefix+"documents/{uid}/versions", h.authenticated(h.listVersions))
	handle("GET "+Prefix+"documents/{uid}/versions/{n}", h.authenticated(h.showVersion))
	handle("GET "+Prefix+"documents/{uid}/diff", h.authenticated(h.diffDocument))
	handle("GET "+Prefix+"documents/{uid}/events", h.authenticated(h.documentEvents))
	handle("GET "+Prefix+"documents/{uid}/transitions", h.authenticated(h.listTransitions))
	handle("POST "+Prefix+"documents/{uid}/transitions", h.authenticated(h.doTransition))

	handle("GET "+Prefix+"element-types", h.authenticated(h.listElementTypes))
	handle("GET "+Prefix+"workflows", h.authenticated(h.listWorkflows))
}

// unavailable answers a request that reached a route built without a service.
//
// Nothing serves such a table today -- "cmsd serve" opens the database before
// it builds anything, and refuses to start if it cannot (invariant 21) -- so
// this is a guard against a wiring mistake rather than a supported mode.
func unavailable(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, Problem{
		Type:   problemBase + "unavailable",
		Title:  "Not available",
		Status: http.StatusServiceUnavailable,
		Detail: "this server was built without a database",
	})
}
