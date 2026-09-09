// Copyright (c) 2026 Michael D Henderson.

package events

// The event vocabulary (DESIGN.md 10).
//
// Event types are Go constants with display names in a registry here, rather
// than a seeded table of rows. The system we learned from used four tables and
// 153 seeded rows to say what this map says; one table with a JSON payload and
// a registry in code does the same work, and the admin screens list them from
// here rather than from a SELECT.
//
// The naming is "subject.verb-in-the-past", lowercase, dotted. It is a
// convention rather than a constraint the schema enforces, which is exactly
// why it is written down.
const (
	// UserCreated is written when a user comes into existence. In M2 that is
	// only "cmsdb bootstrap admin".
	UserCreated = "user.created"

	// SessionCreated is written when a password login succeeds.
	SessionCreated = "session.created"

	// SessionEnded is written when a session is deleted, which is what
	// logging out is.
	SessionEnded = "session.ended"

	// SessionDevLogin is written by GET /__development/log-me-in/{email}
	// (DESIGN.md 11). Every state change writes an event, and this is a state
	// change; it is also exactly the line an operator wants during an
	// incident, which is why the payload carries the email and the peer.
	SessionDevLogin = "session.dev_login"

	// GrantCreated is written when a grant is added to a role.
	GrantCreated = "grant.created"

	// RoleAssigned is written when a user is given a role.
	RoleAssigned = "role.assigned"

	// The document lifecycle (PLAN.md M3). Every one of these is written in
	// the same transaction as the change it records, which is what makes a
	// document's history a query rather than a hope (invariant 7).

	// DocumentCreated is written when a document comes into existence,
	// together with its first working draft.
	DocumentCreated = "document.created"

	// DocumentCheckedOut is written when the edit lease is taken. The payload
	// carries the version being edited and when the lease runs out, because
	// "who has this and until when" is the question asked about a document
	// nobody can edit.
	DocumentCheckedOut = "document.checked_out"

	// DocumentDraftUpdated is written when the open working draft changes. It
	// names the fields that changed and never their contents: a document body
	// is not something this system logs (DESIGN.md 14).
	DocumentDraftUpdated = "document.draft_updated"

	// DocumentCheckedIn is written when a draft becomes an immutable version.
	DocumentCheckedIn = "document.checked_in"

	// DocumentCheckoutCanceled is written when the lease is released without
	// checking in. The draft survives; releasing a lease is not discarding
	// work, which is what DocumentReverted is for.
	DocumentCheckoutCanceled = "document.checkout_canceled"

	// DocumentReverted is written when a draft is discarded. The payload says
	// whether the document survived it: a document whose draft was its only
	// version is deleted, and that deletion has to be reconstructible from
	// the event, because the row it was about is gone.
	DocumentReverted = "document.reverted"

	// DocumentTransitioned is written when a document moves between workflow
	// states (PLAN.md M4). It is the only event that records a change to
	// documents.state, because internal/workflow is the only thing that makes
	// one (invariant 4).
	//
	// The payload carries where it went from and to, which transition, the
	// note, and the guards and effects the transition declared -- enough to
	// reconstruct not just that it moved but what the process demanded of it
	// at the time. A workflow reconfigured next month does not rewrite what
	// happened this month.
	DocumentTransitioned = "document.transitioned"

	// The assignment events (PLAN.md M5 acceptance 2). Assignment and due
	// dates are properties of the document row rather than of a version, so
	// changing one is not a check-in and not a transition -- and it still
	// writes an event, because "who was given this, by whom, and when was it
	// due" is the question asked about work that did not get done.

	// DocumentAssigned is written when a document is given to somebody. The
	// payload names the assignee, and the due date when the same request set
	// one: handing work over with a deadline is one act and one event.
	DocumentAssigned = "document.assigned"

	// DocumentUnassigned is written when a document is taken off somebody's
	// list without being given to anybody else. It is its own type rather
	// than an assignment to nobody, because "returned to the pile" is what a
	// person reading a history is looking for.
	DocumentUnassigned = "document.unassigned"

	// DocumentDueChanged is written when the deadline moves without the
	// assignee changing. The payload carries the new date, or says it was
	// cleared.
	DocumentDueChanged = "document.due_changed"

	// The job events (PLAN.md M6). They record what became of a piece of
	// scheduled work, which is the question asked when something that should
	// have been published is not there.
	//
	// There are five of them and not seven, and the two that are missing are
	// the interesting decision. A claim and a lease extension are not written
	// as events: a lease is not durable state about the world, it is a
	// deadline that expires on its own, and the row already carries the whole
	// of it -- lease_owner, lease_expires_at, and the attempts counter the
	// claim itself increments. Writing a row per claim would cost an event
	// per attempt of every job in a fifty-thousand document republish to
	// record something no longer true one lease later. What invariant 7 asks
	// for is that an operation's effect be reconstructible, and every durable
	// effect a job has -- it was scheduled, it worked, an attempt failed, it
	// was given up on, somebody put it back -- has an event below.

	// JobEnqueued is written when a job is scheduled. The payload carries the
	// kind, the priority, and when it may start; it never carries the
	// payload, which is the handler's argument and may name anything.
	JobEnqueued = "job.enqueued"

	// JobCompleted is written when a handler returns without an error. The
	// payload names the worker that ran it and which attempt succeeded.
	JobCompleted = "job.completed"

	// JobFailed is written when an attempt fails and another is allowed. The
	// payload carries the error, the attempt number, and when the retry is
	// scheduled for.
	JobFailed = "job.failed"

	// JobAbandoned is written when the last allowed attempt fails. It is a
	// separate type from JobFailed because "it is being retried" and "nobody
	// is going to try again" are the two different things a person reading a
	// queue needs to tell apart, and burying the difference in a payload
	// field is how the second one gets missed.
	JobAbandoned = "job.abandoned"

	// JobRetried is written when somebody puts an abandoned job back on the
	// queue. The payload records the failure it is being retried out of, so
	// that the reason survives the columns the retry clears.
	JobRetried = "job.retried"

	// The structure events (PLAN.md M7). Categories, output channels and
	// element types are configuration rather than content, and every one of
	// these is a state change that a document's address or validity depends
	// on -- which is exactly why they are events. "Why did every URL under
	// /features change last Tuesday" is a question about the category tree,
	// and without these the answer is not in the database.

	// CategoryCreated is written when a category comes into existence. A
	// site's root is not one of them: it is created in the same transaction
	// as its site and has no separate existence to record.
	CategoryCreated = "category.created"

	// CategoryMoved is written when a category is moved or renamed. The
	// payload carries the old path and the new one and how many rows the
	// subtree rewrite touched, because every URI under it has just changed
	// and the count is what says how much.
	CategoryMoved = "category.moved"

	// CategoryDeleted is written before the row goes, against the id it names,
	// so that a deleted category still has a history.
	CategoryDeleted = "category.deleted"

	// DocumentFiled is written when a document's categories change. It names
	// the primary category, because that is the one the URI is built from:
	// refiling a story moves its address, and this is the line that says when.
	DocumentFiled = "document.filed"

	// OutputChannelCreated and OutputChannelUpdated record a change to where
	// content goes and what its address looks like. The payload carries the
	// URI formats verbatim: a format edited last month is what last month's
	// addresses were built from, and a configuration row shows only what it
	// says today.
	OutputChannelCreated = "output_channel.created"
	OutputChannelUpdated = "output_channel.updated"

	// ElementTypeCreated and ElementTypeUpdated record a change to what fields
	// a document carries. The payload names the fields rather than carrying
	// the whole schema, for the reason a draft update names fields and not
	// contents.
	ElementTypeCreated = "element_type.created"
	ElementTypeUpdated = "element_type.updated"
)

// names are the display names the admin screens and the CLI show. A type with
// no entry here is a bug -- Registered reports it, and a test asserts every
// constant above is present.
var names = map[string]string{
	UserCreated:     "User created",
	SessionCreated:  "Signed in",
	SessionEnded:    "Signed out",
	SessionDevLogin: "Signed in without a password (development)",
	GrantCreated:    "Grant created",
	RoleAssigned:    "Role assigned",

	DocumentCreated:          "Document created",
	DocumentCheckedOut:       "Checked out",
	DocumentDraftUpdated:     "Draft edited",
	DocumentCheckedIn:        "Checked in",
	DocumentCheckoutCanceled: "Checkout cancelled",
	DocumentReverted:         "Draft reverted",
	DocumentTransitioned:     "Moved",
	DocumentAssigned:         "Assigned",
	DocumentUnassigned:       "Unassigned",
	DocumentDueChanged:       "Due date changed",

	JobEnqueued:  "Job scheduled",
	JobCompleted: "Job completed",
	JobFailed:    "Job attempt failed",
	JobAbandoned: "Job abandoned",
	JobRetried:   "Job retried",

	CategoryCreated:      "Category created",
	CategoryMoved:        "Category moved",
	CategoryDeleted:      "Category deleted",
	DocumentFiled:        "Filed",
	OutputChannelCreated: "Output channel created",
	OutputChannelUpdated: "Output channel updated",
	ElementTypeCreated:   "Element type created",
	ElementTypeUpdated:   "Element type updated",
}

// All returns every event type this binary knows, in a stable order.
func All() []string {
	return []string{
		UserCreated,
		SessionCreated,
		SessionEnded,
		SessionDevLogin,
		GrantCreated,
		RoleAssigned,
		DocumentCreated,
		DocumentCheckedOut,
		DocumentDraftUpdated,
		DocumentCheckedIn,
		DocumentCheckoutCanceled,
		DocumentReverted,
		DocumentTransitioned,
		DocumentAssigned,
		DocumentUnassigned,
		DocumentDueChanged,
		JobEnqueued,
		JobCompleted,
		JobFailed,
		JobAbandoned,
		JobRetried,
		CategoryCreated,
		CategoryMoved,
		CategoryDeleted,
		DocumentFiled,
		OutputChannelCreated,
		OutputChannelUpdated,
		ElementTypeCreated,
		ElementTypeUpdated,
	}
}

// Name returns the display name of an event type, or the type itself when it
// has none. It never returns the empty string: this ends up in a table a
// person reads.
func Name(eventType string) string {
	if n, ok := names[eventType]; ok {
		return n
	}
	return eventType
}

// Registered reports whether an event type is one this binary knows about.
func Registered(eventType string) bool {
	_, ok := names[eventType]
	return ok
}
