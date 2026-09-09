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
// that carries the anti-escalation check; M3 added the document, version,
// diff, and history routes, M4 the transitions, M5 assignment, due dates, and
// the queues, M6 the job queue, M7 sites, categories, output channels,
// element types, and the URIs they produce, M8 the preview, M9 the
// publications and the resources they leave behind, and M11 the comments and
// approvals the guards have been reading since M4. The routes that are still
// missing arrive with the milestones that implement them; a route registered
// before its service method exists is a 501 nobody asked for.
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

	// Assignment, due dates, and the saved queues (PLAN.md M5). Assignment is
	// a subresource for the reason transitions are: POST hands the work over,
	// DELETE takes it back, and there is no PATCH that sets an assignee among
	// a dozen other fields.
	//
	// Two of these are additions to DESIGN.md 12's list. The due-date
	// subresource is one, because the design put a due date on PATCH
	// /documents/{uid} and that route writes the working draft and needs the
	// edit lease -- requiring a checkout to set a deadline would mean taking
	// the draft away from the person the deadline is for. GET /queues is the
	// other: the design names only /queues/{slug}, and a client that cannot
	// ask which queues exist has to be told out of band.
	handle("POST "+Prefix+"documents/{uid}/assignment", h.authenticated(h.assignDocument))
	handle("DELETE "+Prefix+"documents/{uid}/assignment", h.authenticated(h.unassignDocument))
	handle("PUT "+Prefix+"documents/{uid}/due", h.authenticated(h.setDue))
	handle("DELETE "+Prefix+"documents/{uid}/due", h.authenticated(h.clearDue))
	handle("GET "+Prefix+"queues", h.authenticated(h.listQueues))
	handle("GET "+Prefix+"queues/{slug}", h.authenticated(h.showQueue))

	// The job queue (PLAN.md M6). GET reads it; POST .../retry puts an
	// abandoned job back on it. There is deliberately no route that creates a
	// job: work is scheduled by the operation that needs it, and a route
	// taking a kind and a payload would run any handler in the binary with
	// arguments the client chose.
	//
	// The path speaks uid rather than the {id} of DESIGN.md 12, because
	// invariant 10 says the API speaks uid only and an invariant outranks a
	// path spelled in an example. Jobs carry one, as of migration 0008.
	handle("GET "+Prefix+"jobs", h.authenticated(h.listJobs))
	handle("POST "+Prefix+"jobs/{uid}/retry", h.authenticated(h.retryJob))

	// Sites, categories, output channels, and element types (PLAN.md M7).
	// DESIGN.md 12's last line names all four as ordinary CRUD collections and
	// this is them, less the parts nothing needs yet: there is no DELETE on an
	// output channel or an element type, because deleting one would orphan
	// every document that points at it and the schema says so with a foreign
	// key rather than with a cascade.
	//
	// A category is addressed by uid, like everything else the API mutates
	// (invariant 10), and named by path everywhere a person types one:
	// "/features/film/" is what a grant carries and what a URI is built from,
	// and UNIQUE (site_id, path) is what makes it a lookup key.
	handle("GET "+Prefix+"sites", h.authenticated(h.listSites))
	handle("GET "+Prefix+"categories", h.authenticated(h.listCategories))
	handle("POST "+Prefix+"categories", h.authenticated(h.createCategory))
	handle("GET "+Prefix+"categories/{uid}", h.authenticated(h.showCategory))
	handle("PATCH "+Prefix+"categories/{uid}", h.authenticated(h.patchCategory))
	handle("DELETE "+Prefix+"categories/{uid}", h.authenticated(h.deleteCategory))

	handle("GET "+Prefix+"output-channels", h.authenticated(h.listOutputChannels))
	handle("POST "+Prefix+"output-channels", h.authenticated(h.createOutputChannel))
	handle("GET "+Prefix+"output-channels/{uid}", h.authenticated(h.showOutputChannel))
	handle("PATCH "+Prefix+"output-channels/{uid}", h.authenticated(h.patchOutputChannel))

	// Where a document is filed, and the addresses that follow from it.
	//
	// Categories are a subresource rather than a field on PATCH
	// /documents/{uid}, which is a departure from DESIGN.md 12's original list
	// and the same one M5 made for the due date: that route writes the working
	// draft and needs the edit lease, and document_categories rows point at
	// the document rather than at a version, so there is no draft copy of a
	// filing to protect. DESIGN.md 12 has been corrected to match.
	handle("GET "+Prefix+"documents/{uid}/categories", h.authenticated(h.listDocumentCategories))
	handle("PUT "+Prefix+"documents/{uid}/categories", h.authenticated(h.fileDocument))
	handle("GET "+Prefix+"documents/{uid}/uris", h.authenticated(h.documentURIs))

	// Rendering (PLAN.md M8). POST because it produces something: a file in
	// the scratch tree, at an address of its own, which GET /preview/{name}
	// serves. The same route validates the template instead, with
	// {"validate": true}, because "which template would this document use"
	// and "does that template compile" are one lookup and the answer to the
	// second is worthless without the first.
	//
	// The mount that serves what this writes is registered by
	// internal/server, through RegisterPreview: it is not under Prefix, and a
	// route table that called it "json api" would be describing it wrongly.
	handle("POST "+Prefix+"documents/{uid}/preview", h.authenticated(h.previewDocument))

	// Publishing (PLAN.md M9). POST schedules a publish and answers 202: what
	// it created is a job, and the page it names does not exist yet. GET
	// .../resources says what is at the document's addresses now, which is
	// the visible face of published_resources and the only way to see that a
	// slug change took the old file with it.
	//
	// A publish is scheduled and never performed inline, even when it is
	// wanted now. "Publish at midnight" and "publish now" are the same request
	// with a different instant in it, and that is only true if the request
	// never does the work itself (DESIGN.md 8.1).
	handle("POST "+Prefix+"documents/{uid}/publications", h.authenticated(h.publishDocument))
	handle("GET "+Prefix+"documents/{uid}/resources", h.authenticated(h.documentResources))

	// Comments and approvals (PLAN.md M11). Both tables have been in the
	// schema since M4, with the guards that read them enforced; these are the
	// routes that let a person write one.
	//
	// Two of the five are additions to DESIGN.md 12's list, for the reason
	// M5's due-date subresource was. GET .../approvals is one: a client that
	// can record an approval and cannot read one has to infer the count from a
	// transition refusal, and it is what makes an older version's approvals
	// visible after a check-in has dropped the live count to zero. The
	// {uid} on the resolution route is a comment's rather than a document's,
	// which is DESIGN.md 12 exactly: resolving a thread is an act on the
	// thread.
	//
	// There is deliberately no route that removes a comment and none that
	// withdraws somebody else's approval. A discussion is an audit record and
	// an approval is a statement by one person; an editor who disagrees moves
	// the document, and EffectClearApprovals is how a process discards them.
	handle("GET "+Prefix+"documents/{uid}/comments", h.authenticated(h.listComments))
	handle("POST "+Prefix+"documents/{uid}/comments", h.authenticated(h.createComment))
	handle("POST "+Prefix+"comments/{uid}/resolution", h.authenticated(h.resolveComment))
	handle("GET "+Prefix+"documents/{uid}/approvals", h.authenticated(h.listApprovals))
	handle("POST "+Prefix+"documents/{uid}/approvals", h.authenticated(h.approveDocument))
	handle("DELETE "+Prefix+"documents/{uid}/approvals/current", h.authenticated(h.withdrawApproval))

	handle("GET "+Prefix+"element-types", h.authenticated(h.listElementTypes))
	handle("POST "+Prefix+"element-types", h.authenticated(h.createElementType))
	handle("GET "+Prefix+"element-types/{key}", h.authenticated(h.showElementType))
	handle("PATCH "+Prefix+"element-types/{key}", h.authenticated(h.patchElementType))
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
