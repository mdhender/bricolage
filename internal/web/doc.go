// Copyright (c) 2026 Michael D Henderson.

// Package web is the HTMX transport (PLAN.md M13): html/template handlers
// rendering pages and fragments by calling the same service methods
// internal/api serialises to JSON. It is a client of the application, not the
// application (DESIGN.md 3).
//
// Permitted imports: internal/{service,domain,config,reqctx,edge,workflow,events}.
// The last three need a word each. internal/edge is the one status mapping and
// the one cookie-writing path both transports share (DESIGN.md 4).
// internal/workflow is imported for the Allowed type its Available returns --
// rendering the action bar is the whole reason that type carries a reason and
// a guard name. internal/events is the event vocabulary's display-name
// registry: DESIGN.md 10 says the screens list event types from code rather
// than from a SELECT, and a history is one of those screens. All three are
// leaves from here, so the dependency still points downward.
//
// This package never imports internal/store and never writes SQL
// (PLAN.md M13 acceptance 1, enforced by a test in this package).
//
// # The action bar is Available, rendered
//
// Every button on a document is one entry of workflow.Available, and a refused
// entry is drawn disabled with the reason beside it rather than left out
// (invariant 5, PLAN.md M13 acceptance 2). A menu drawn from a second opinion
// about what is permitted is the defect this system was built to avoid: in the
// system we learnt from, the check lived only in the template that drew the
// menu, so a forged POST performed a transition nobody was allowed to make.
// Here the template draws what Available says and the POST goes through
// service.Transition, which asks the engine again inside the transaction --
// so a forged POST for a refused move is a 409 and not a state change.
//
// # This package performs nothing earl cannot
//
// Every mutation here is one service method, and every one of those methods is
// behind a JSON route earl already drives (PLAN.md M13 acceptance 5). The UI
// is a second face on one application rather than a second application; a
// screen that could do something no client could would be a use case that
// escaped the service layer.
//
// # The templates are embedded, and parsed once
//
// DESIGN.md 14's "re-read from disk per request in development" is about the
// content template tree, which internal/render reloads: those are edited by
// the people using the system while it runs. These are the program's own
// screens, they ship inside the binary, and a UI that could be changed by
// editing a file next to the binary is a UI whose behaviour depends on what is
// in a directory nobody deployed.
package web
