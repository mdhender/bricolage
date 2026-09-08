// Copyright (c) 2026 Michael D Henderson.

package domain

import "fmt"

// Privilege is a point on the ordered access scale (DESIGN.md 7).
//
// Two properties are load-bearing and neither may be weakened:
//
//   - It is ordered and cumulative, so "may they do this?" is one integer
//     comparison rather than a set membership test over a permission list.
//   - Deny is 255, the numeric maximum, so effective privilege is MAX over the
//     matching grants and deny-overrides falls out for free. There is no
//     second pass and no special case in the resolver.
//
// The scale is kept from the system we learned from because it is well chosen.
// Recall in particular is worth keeping as a distinct level: it is the one
// genuinely workflow-shaped privilege, and generic create/read/update/delete
// permission sets have no equivalent.
type Privilege uint8

const (
	// NoPrivilege is the absence of any grant. It is not a level somebody can
	// be given; it is what Resolve returns when nothing matched.
	NoPrivilege Privilege = 0

	Read    Privilege = 1 // view
	Edit    Privilege = 2 // modify
	Recall  Privilege = 3 // pull back out of a terminal state
	Create  Privilege = 4 // create new
	Publish Privilege = 5 // publish

	// Deny is a veto. It is the numeric maximum so that MAX resolves it, and
	// it is not comparable to the levels below it: a subject resolving to Deny
	// may do nothing at all, whatever the number suggests. Ask with Allows.
	Deny Privilege = 255
)

// privilegeNames is the name of each level, for the API, the CLI, and the
// admin screens. Deny is included: it is a value a grant may carry.
var privilegeNames = map[Privilege]string{
	NoPrivilege: "none",
	Read:        "read",
	Edit:        "edit",
	Recall:      "recall",
	Create:      "create",
	Publish:     "publish",
	Deny:        "deny",
}

// Privileges are the levels a grant may carry, in scale order. NoPrivilege is
// absent: it is an answer, not a grant.
var Privileges = []Privilege{Read, Edit, Recall, Create, Publish, Deny}

// String returns the name of the level, or a numeric form for a value that is
// not on the scale. It never panics: this ends up in error messages.
func (p Privilege) String() string {
	if name, ok := privilegeNames[p]; ok {
		return name
	}
	return fmt.Sprintf("privilege(%d)", uint8(p))
}

// Valid reports whether p is a level a grant may carry.
func (p Privilege) Valid() bool {
	_, ok := privilegeNames[p]
	return ok && p != NoPrivilege
}

// Allows reports whether an effective privilege of p permits an operation
// needing want.
//
// This is the only place the Deny special case lives, and it is the reason
// callers ask this rather than writing "p >= want" themselves. Deny is 255 so
// that MAX resolves it; that same 255 would satisfy every comparison if
// anybody compared it directly, which would turn a veto into universal
// permission. One function, one rule, no second implementation.
func (p Privilege) Allows(want Privilege) bool {
	if p == Deny {
		return false
	}
	return p >= want
}

// ParsePrivilege maps a name to a level. It accepts exactly the names String
// produces, because a permission that can be spelled two ways is a permission
// somebody will spell the third way.
func ParsePrivilege(s string) (Privilege, error) {
	for p, name := range privilegeNames {
		if name == s && p != NoPrivilege {
			return p, nil
		}
	}
	return NoPrivilege, fmt.Errorf("%q: not a privilege (read, edit, recall, create, publish, deny): %w", s, ErrInvalid)
}
