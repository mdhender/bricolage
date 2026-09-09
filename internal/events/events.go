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

	// The publishing events (PLAN.md M9). Between them they answer the
	// question a publishing system exists to be asked: what is at this
	// address, which version put it there, and when did the one before it go
	// away.

	// DocumentPublishScheduled is written when somebody asks for a publish.
	// The payload carries the version it pinned and the instant it is
	// scheduled for, which is the whole of invariant 8 written down at the
	// moment the promise is made: an editor reading this line a week later
	// can see that version 5 was scheduled, whatever the document has become.
	DocumentPublishScheduled = "document.publish_scheduled"

	// DocumentPublished is written when a publish job succeeds. The payload
	// names the version, the channels, and every address written; it does not
	// carry the bytes, for the reason a draft update names fields and not
	// contents.
	DocumentPublished = "document.published"

	// ResourceExpired is written when a file the publisher no longer produces
	// is deleted. Its subject is the document rather than the resource,
	// because the resource row was deleted by the publish that stopped
	// producing the address and an audit trail whose subject can vanish is an
	// audit trail with holes in it.
	ResourceExpired = "resource.expired"

	// The collaboration events (PLAN.md M11 acceptance 6). Their subject is
	// the document rather than the comment or the approval, because "what
	// happened to this story" is the question a person asks and a thread that
	// only appears in its own history is a thread nobody finds. The payload
	// carries the comment's uid, so the row still names the thing it was
	// about.

	// DocumentCommented is written when somebody opens a thread or replies to
	// one. The payload names the thread and whether this was a reply; it
	// carries the body, because a comment *is* what somebody said and a
	// history that recorded only that somebody spoke would be useless. That
	// is not the exception DESIGN.md 14 forbids: a document body is content
	// this system stores versions of, and a remark about it is not.
	DocumentCommented = "document.commented"

	// DocumentCommentResolved is written when a thread is closed. It is what
	// turns "the comments_resolved guard refused" into a question with an
	// answer: who decided this was settled, and when.
	DocumentCommentResolved = "document.comment_resolved"

	// DocumentApproved is written when somebody signs off on a version in a
	// state. The payload pins the version, because that is what the approval
	// is about and what invalidates it: an approval of version 4 says nothing
	// about version 5, and the event has to be readable after both exist.
	//
	// Approving twice is idempotent and writes one event, not two. Nothing
	// changed the second time, and invariant 7 asks for an event per state
	// change rather than per request.
	DocumentApproved = "document.approved"

	// DocumentApprovalWithdrawn is written when somebody takes their sign-off
	// back. It is its own type rather than a payload flag on the one above,
	// because "who has approved this" is answered by reading the history
	// forwards and a withdrawal that looked like an approval would answer it
	// wrongly.
	DocumentApprovalWithdrawn = "document.approval_withdrawn"

	// The alert events (PLAN.md M12). A rule decides who is told about
	// everything else in this list, so a change to one is a change to what
	// the system says out loud -- which is exactly the kind of configuration
	// change the structure events below exist to record.

	// AlertRuleCreated is written when a rule comes into existence. The
	// payload carries the event type it watches, the channel, and the target,
	// because "who was being told about this, last March" is the question an
	// audit of a notification asks.
	AlertRuleCreated = "alert_rule.created"

	// AlertRuleUpdated is written when a rule's configuration changes,
	// turning one off included. A rule that stopped firing is the hardest
	// kind of failure to notice, and this is the line that says when it
	// stopped.
	AlertRuleUpdated = "alert_rule.updated"

	// AlertRuleDeleted is written before the row goes, against the id it
	// names, so that a deleted rule still has a history. The notifications it
	// produced outlive it -- notifications.rule_id is ON DELETE SET NULL --
	// and this is what still says what produced them.
	AlertRuleDeleted = "alert_rule.deleted"

	// There is deliberately no "notification.read". Marking one's own
	// notification read is a state change, and it is the same kind of state
	// change a job claim is: the row carries the whole of it in read_at,
	// nobody but its owner can make it, and an event per read would put a row
	// in the audit spine every time somebody scrolled an inbox. What
	// invariant 7 asks is that an operation's effect be reconstructible, and
	// the effect is one nullable column on the row itself.

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

	// Invitations (issue #6). Registration is invite-only, so these four are
	// the history of how every account but the bootstrap administrator came
	// to exist.
	//
	// There is deliberately no "invitation.expired". Expiry is derived from
	// expires_at, so nothing runs at the 48-hour mark to write an event, and
	// an event nothing writes is worse than no event at all: an alert rule
	// could be pointed at it and would never fire.
	InvitationCreated = "invitation.created"

	// InvitationRedeemed is written in the same transaction as the user row it
	// created, beside the user.created that records the account itself.
	InvitationRedeemed = "invitation.redeemed"

	// InvitationRevoked is written when an administrator cancels one. Its
	// payload carries the reason, when one was given, which is why there is no
	// separate verb for forcing an invitation to expire: the two acts differ
	// only in the sentence recorded here.
	InvitationRevoked = "invitation.revoked"

	// InvitationSuperseded is written when a later invitation to the same
	// address replaces this one. The actor is whoever created the replacement.
	InvitationSuperseded = "invitation.superseded"
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

	DocumentCommented:         "Commented",
	DocumentCommentResolved:   "Comment resolved",
	DocumentApproved:          "Approved",
	DocumentApprovalWithdrawn: "Approval withdrawn",

	DocumentPublishScheduled: "Publish scheduled",
	DocumentPublished:        "Published",
	ResourceExpired:          "Resource expired",

	AlertRuleCreated: "Alert rule created",
	AlertRuleUpdated: "Alert rule updated",
	AlertRuleDeleted: "Alert rule deleted",

	CategoryCreated:      "Category created",
	CategoryMoved:        "Category moved",
	CategoryDeleted:      "Category deleted",
	DocumentFiled:        "Filed",
	OutputChannelCreated: "Output channel created",
	OutputChannelUpdated: "Output channel updated",
	ElementTypeCreated:   "Element type created",
	ElementTypeUpdated:   "Element type updated",

	InvitationCreated:    "Invitation sent",
	InvitationRedeemed:   "Invitation redeemed",
	InvitationRevoked:    "Invitation revoked",
	InvitationSuperseded: "Invitation superseded",
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
		DocumentCommented,
		DocumentCommentResolved,
		DocumentApproved,
		DocumentApprovalWithdrawn,
		DocumentPublishScheduled,
		DocumentPublished,
		ResourceExpired,
		AlertRuleCreated,
		AlertRuleUpdated,
		AlertRuleDeleted,
		CategoryCreated,
		CategoryMoved,
		CategoryDeleted,
		DocumentFiled,
		OutputChannelCreated,
		OutputChannelUpdated,
		ElementTypeCreated,
		ElementTypeUpdated,
		InvitationCreated,
		InvitationRedeemed,
		InvitationRevoked,
		InvitationSuperseded,
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
