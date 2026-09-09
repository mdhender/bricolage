// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/api"
	"github.com/mdhender/bricolage/internal/edge"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/web"
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

// IsAPI reports whether this route is part of the JSON API.
func (r Route) IsAPI() bool {
	return strings.Contains(r.Pattern, api.Prefix)
}

// IsPreview reports whether this route is the preview mount, which is neither
// the JSON API nor a development affordance.
func (r Route) IsPreview() bool {
	return strings.Contains(r.Pattern, render.PreviewPrefix)
}

// IsUI reports whether this route is one of the HTML UI's. The three
// predicates are what "cmsd routes" groups the table by, and a route that
// answered to none of them would be one nobody had classified.
func (r Route) IsUI() bool {
	return !r.IsAPI() && !r.IsDevelopment() && !r.IsPreview() && r.Pattern != "GET /healthz"
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

	// The JSON API is registered only when there is a service to serve it.
	// "cmsd routes" builds a table without opening a database, and a route
	// whose handler would dereference a nil service is worse than a route that
	// is not there -- but the table would then differ from the one "serve"
	// produces, so routes is told about the service too and the two agree.
	if s.svc != nil || s.declareAPI {
		deps := api.Deps{
			Service:     s.svc,
			Environment: s.env,
			Logger:      s.log,
		}
		b.note = "json api"
		api.Register(b, deps)

		// The preview mount (PLAN.md M8). It is not under the API prefix --
		// what it serves is a page, and the point of a preview is that a
		// browser can be pointed at it -- so it is registered separately and
		// the table calls it what it is. It is registered whether or not this
		// server was given a preview tree: a route that answers "this server
		// was not started with --preview" is more use than a 404 that leaves
		// somebody looking for a typo in the path.
		b.note = "preview"
		api.RegisterPreview(b, deps)

		// The HTML UI (PLAN.md M13). It is registered beside the API and
		// under the same condition, because it is a second face on one
		// application: the pages call the service methods the JSON routes
		// serialise, and a server with a database has both or neither.
		//
		// Its routes sit at the root of the path space rather than under a
		// prefix, which is what makes "/" the dashboard and "/documents/{uid}"
		// a page somebody can be sent a link to. Nothing here is a catch-all:
		// the UI registers "GET /{$}" and the paths it declares, so an
		// unregistered path is still a 404 from the mux.
		b.note = "html ui"
		web.Register(b, web.Deps{
			Service:     s.svc,
			Environment: s.env,
			Origin:      s.origin,
			Logger:      s.log,
		})
		b.note = ""
	}

	if s.env.IsDevelopment() {
		b.note = "development only"
		devroutes.Register(b, s.devDeps())
		b.note = ""
	}

	return b.mux, b.routes
}

// devDeps assembles what the development routes are handed.
//
// Everything they can do is in this struct. log-me-in is registered only when
// there is a service behind it, so a server built without a database -- which
// is what "cmsd routes" does -- has the shutdown route and nothing else, and
// the table says so.
func (s *Server) devDeps() devroutes.Deps {
	deps := devroutes.Deps{
		Logger:   s.log,
		Shutdown: s.RequestShutdown,
	}
	if s.svc == nil && !s.declareAPI {
		return deps
	}

	// The same cookie-writing path the API uses: a session the development
	// route issues is an ordinary session, so it gets an ordinary cookie
	// (invariant 13).
	deps.ValidateReturnTo = s.origin.ValidateReturnTo
	deps.SetSessionCookie = edge.SetSessionCookie

	if s.svc == nil {
		// Route-table mode: the pattern is declared so that "cmsd routes"
		// prints what "serve" would mount, and the handler refuses rather
		// than dereferencing nothing.
		deps.DevLogin = func(context.Context, string, string) (devroutes.Login, error) {
			return devroutes.Login{}, errors.New("no database is open")
		}
		return deps
	}

	deps.DevLogin = func(ctx context.Context, email, peer string) (devroutes.Login, error) {
		result, err := s.svc.DevLogin(ctx, email, peer)
		if err != nil {
			return devroutes.Login{}, err
		}
		return devroutes.Login{
			Token:     result.Token,
			ExpiresAt: result.Session.ExpiresAt,
			UserUID:   result.Identity.User.UID,
			Email:     result.Identity.User.Email,
			Name:      result.Identity.User.Name,
		}, nil
	}
	return deps
}

// healthz answers the reverse proxy and anything else asking whether the
// process is up. It says nothing about the database, which cmsd does not open
// until M1.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
