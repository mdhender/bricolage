// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/service"
)

// StaticPrefix is where the stylesheet and the HTMX runtime are served from.
// It is a constant so that the route table, the layout template, and the tests
// name one string.
const StaticPrefix = "/static/"

// Mux is the part of *http.ServeMux that Register needs.
//
// It is an interface for the same reason api.Mux and devroutes.Mux are: the
// route table "cmsd routes" prints is produced by the same call that registers
// the handlers, so it cannot claim a route the mux does not have.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Deps are what the handlers need from the server that owns them.
type Deps struct {
	// Service is the use-case layer. Handlers parse a request, call one
	// method on it, and render the result; they hold no business logic and
	// never reach the store (DESIGN.md 3, PLAN.md M13 acceptance 1).
	Service *service.Service

	// Environment decides how much of an error reaches the browser
	// (DESIGN.md 14) and whether the banner in the layout says so. It gates
	// no route here: the /__development/* routes are gated at registration,
	// in internal/server, and this package registers none of them.
	Environment config.Environment

	// Origin is the public origin. It is what a returnTo is validated
	// against, which is the whole of this package's defence against an open
	// redirect on the login form.
	Origin config.PublicOrigin

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger
}

// Handler holds what the pages share.
type Handler struct {
	svc    *service.Service
	env    config.Environment
	origin config.PublicOrigin
	log    *slog.Logger

	// pages is one template set per page, each a clone of the layout and the
	// partials with that page's "content" parsed into it, and fragments is
	// the set they were cloned from, which is what an HTMX swap renders one
	// partial out of. Parsing once at construction is deliberate; see this
	// package's doc comment.
	pages     map[string]*template.Template
	fragments *template.Template
}

// New builds the handler set, parsing the templates.
//
// A template that will not parse is a programming error in an embedded file,
// so it panics rather than returning an error: there is no configuration that
// could produce it, no operator action that could fix it, and a server that
// started with half a UI would be a server whose first editor found out.
func New(deps Deps) *Handler {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	origin := deps.Origin
	if origin.URL == nil {
		// The zero origin would make returnTo validation dereference nothing.
		// internal/server always supplies one; this is the default it would
		// have supplied, so that a handler set built directly by a test
		// refuses an off-origin redirect rather than panicking on it.
		if parsed, err := config.ParsePublicOrigin(config.DefaultPublicOrigin); err == nil {
			origin = parsed
		}
	}
	pages, fragments := mustParse()
	return &Handler{
		svc:       deps.Service,
		env:       deps.Environment,
		origin:    origin,
		log:       log,
		pages:     pages,
		fragments: fragments,
	}
}

// Register adds the HTML routes to mux (PLAN.md M13).
//
// The root pattern is "GET /{$}" and not "GET /": the UI answers the paths it
// declares, and a catch-all would turn every mistyped URL into the dashboard
// and every unregistered API path into an HTML page. An unknown path is a 404
// from the mux, which is the same answer the route table gives.
//
// Where a form performs what the JSON API spells with DELETE or PUT, the path
// carries the verb instead -- ".../checkout/cancel", ".../approvals/withdraw"
// -- because an HTML form may only GET or POST, and the alternative is a
// hidden _method field, which is a second way of saying what the method
// already says.
func Register(mux Mux, deps Deps) {
	h := New(deps)

	// With no service there is no database, and the routes are registered
	// anyway, answering 503. That is for "cmsd routes", whose job is to print
	// the table "serve" would mount (DESIGN.md 11); it is the rule
	// internal/api follows, for the reason given there.
	handle := func(pattern string, handler http.Handler) {
		if deps.Service == nil {
			handler = http.HandlerFunc(h.unavailable)
		}
		mux.Handle(pattern, handler)
	}

	// The stylesheet and the HTMX runtime, from the embedded tree. They are
	// registered whether or not there is a service: they need none, and a UI
	// whose CSS 503s would be a stranger sight than one whose pages do.
	mux.Handle("GET "+StaticPrefix+"{file}", http.HandlerFunc(serveStatic))

	handle("GET /login", http.HandlerFunc(h.loginForm))
	handle("POST /login", http.HandlerFunc(h.login))
	handle("POST /logout", h.page(h.logout))

	handle("GET /{$}", h.page(h.dashboard))
	handle("GET /queues", h.page(h.queues))
	handle("GET /queues/{slug}", h.page(h.queue))

	// Documents: the list, the editor, and the lease operations. Check-out,
	// cancel, check-in and revert are four buttons because they are four
	// different acts -- cancelling says "I am not editing this now" and
	// reverting says "throw away what I wrote" (DESIGN.md 12).
	handle("GET /documents", h.page(h.documents))
	handle("GET /documents/new", h.page(h.newDocumentForm))
	handle("POST /documents", h.page(h.createDocument))
	handle("GET /documents/{uid}", h.page(h.document))
	handle("GET /documents/{uid}/edit", h.page(h.editor))
	handle("POST /documents/{uid}/edit", h.page(h.saveDraft))
	handle("POST /documents/{uid}/checkout", h.page(h.checkout))
	handle("POST /documents/{uid}/checkout/cancel", h.page(h.cancelCheckout))
	handle("POST /documents/{uid}/checkin", h.page(h.checkin))
	handle("POST /documents/{uid}/revert", h.page(h.revert))

	// The desk actions. One POST, drawn from Available, refused by the same
	// check that drew it (invariant 5).
	handle("POST /documents/{uid}/transitions", h.page(h.transition))

	// Assignment, the due date, and the filing: the three subresources that
	// are properties of the document row rather than of the working draft,
	// and deliberately need no edit lease (DESIGN.md 12).
	handle("POST /documents/{uid}/assignment", h.page(h.assign))
	handle("POST /documents/{uid}/assignment/clear", h.page(h.unassign))
	handle("POST /documents/{uid}/due", h.page(h.setDue))
	handle("POST /documents/{uid}/categories", h.page(h.fileDocument))

	// History: the versions, the events, and the word-level diff between two
	// versions.
	handle("GET /documents/{uid}/history", h.page(h.history))
	handle("GET /documents/{uid}/diff", h.page(h.diff))

	// The discussion and the sign-offs.
	handle("POST /documents/{uid}/comments", h.page(h.comment))
	handle("POST /comments/{uid}/resolution", h.page(h.resolveComment))
	handle("POST /documents/{uid}/approvals", h.page(h.approve))
	handle("POST /documents/{uid}/approvals/withdraw", h.page(h.withdrawApproval))

	// Rendering and publishing.
	handle("POST /documents/{uid}/preview", h.page(h.preview))
	handle("POST /documents/{uid}/publications", h.page(h.publish))

	// The inbox. It is the caller's own and nobody else's; there is no route
	// here to another person's, for the reason there is none in the API.
	handle("GET /notifications", h.page(h.notifications))
	handle("POST /notifications/{uid}/read", h.page(h.readNotification))

	// The job queue, which is the visible half of what publishing schedules.
	handle("GET /jobs", h.page(h.jobs))
	handle("POST /jobs/{uid}/retry", h.page(h.retryJob))

	// Administration: the workflows a document may be in, the structure it is
	// filed against, and the two writes that hand out access. Both writes
	// carry the anti-escalation refusal, because it lives in the service and
	// not in the screen (invariant 12).
	handle("GET /admin", h.page(h.admin))
	handle("POST /admin/grants", h.page(h.createGrant))
	handle("POST /admin/roles", h.page(h.assignRole))

	// Invitations and the account list (issue #6). Registration is invite-only,
	// so these two screens are how everybody but the bootstrap administrator
	// comes to exist -- and the account list is what makes the role form above
	// usable, since it needs a uid and nothing else here produced one.
	//
	// Two verbs and no more: create and revoke. There is deliberately no button
	// that extends an invitation, renews an expired one, or forces one to
	// expire (internal/service/invitations.go says why).
	handle("GET /admin/invitations", h.page(h.invitations))
	handle("POST /admin/invitations", h.page(h.createInvitation))
	handle("POST /admin/invitations/{uid}/revoke", h.page(h.revokeInvitation))
	handle("GET /admin/users", h.page(h.users))

	// Where an invitation link lands. These are the only routes in this package
	// besides the login form that render without a session, and the path comes
	// from config so that the link internal/service mints and the route that
	// answers it cannot drift.
	//
	// The POST has no {token} in its path: the link has to carry the credential,
	// a form does not, so it travels in a hidden field. Redemption issues no
	// session and ends at /login (issue #6).
	handle("GET "+invitePath+"{token}", http.HandlerFunc(h.redeemForm))
	handle("POST "+strings.TrimSuffix(invitePath, "/"), http.HandlerFunc(h.redeem))
}
