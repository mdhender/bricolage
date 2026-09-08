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
