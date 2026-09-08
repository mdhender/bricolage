// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/web/devroutes"
)

// Route is one entry in the table "cmsd routes" prints.
type Route struct {
	// Pattern is the net/http.ServeMux pattern, method included.
	Pattern string

	// Note is a short description for the operator reading the table.
	Note string
}

// IsDevelopment reports whether this route is one of the development
// affordances. The path prefix is the marker, which keeps the answer derivable
// from the pattern rather than from a flag someone has to remember to set.
func (r Route) IsDevelopment() bool {
	return strings.Contains(r.Pattern, devroutes.Prefix)
}

// builder registers handlers on a mux and records what it registered.
//
// Recording as we register is the point: the table cannot claim a route the mux
// does not have, and it cannot omit one the mux does. It satisfies
// devroutes.Mux, so the development routes go through the same path.
type builder struct {
	mux    *http.ServeMux
	routes []Route
	note   string // applied to the next Handle call made through devroutes.Mux
}

// Handle implements devroutes.Mux.
func (b *builder) Handle(pattern string, handler http.Handler) {
	b.routes = append(b.routes, Route{Pattern: pattern, Note: b.note})
	b.mux.Handle(pattern, handler)
}

// handle registers a route with a note of its own.
func (b *builder) handle(pattern, note string, handler http.Handler) {
	b.routes = append(b.routes, Route{Pattern: pattern, Note: note})
	b.mux.Handle(pattern, handler)
}

// buildRoutes assembles the route table for one configuration.
//
// The single conditional in this function is the entire gate on the
// development affordances. It is a registration-time decision, not a
// middleware that matches and then refuses, so an unregistered route is absent
// from the mux and returns 404 because nothing answers it (invariant 16).
func (s *Server) buildRoutes() (*http.ServeMux, []Route) {
	b := &builder{mux: http.NewServeMux()}

	b.handle("GET /healthz", "liveness", http.HandlerFunc(healthz))

	if s.env.IsDevelopment() {
		b.note = "development only"
		devroutes.Register(b, devroutes.Deps{
			Logger:   s.log,
			Shutdown: s.RequestShutdown,
		})
		b.note = ""
	}

	return b.mux, b.routes
}

// healthz answers the reverse proxy and anything else asking whether the
// process is up. It says nothing about the database, which cmsd does not open
// until M1.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
