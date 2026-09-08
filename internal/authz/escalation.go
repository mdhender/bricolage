// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"fmt"

	"github.com/mdhender/bricolage/internal/domain"
)

// ErrEscalation is what the grant-writing path returns when the actor asked
// for more than they hold. It wraps domain.ErrForbidden, so the transport edge
// maps it to 403 with the one mapping function it already has.
var ErrEscalation = fmt.Errorf("%w: nobody may grant a privilege they do not hold over that scope", domain.ErrForbidden)

// Delegable is the highest privilege an actor holding these grants may confer
// over the whole of scope s (DESIGN.md 7.3).
//
// Two rules, and the second is the one that is easy to leave out:
//
//   - Only grants whose scope Covers s count. A grant over one site says
//     nothing about a scope that spans every site, and a resolver that treated
//     it as if it did would let a site editor write a global grant. This is
//     why the check is scope containment rather than Resolve against a
//     representative subject: there is no subject that represents "every
//     document a wildcard reaches".
//   - A Deny anywhere that could touch s takes the answer to none. A veto over
//     part of a scope is a veto over granting anything across it, because the
//     grant being written would apply where the actor themselves may not act.
func Delegable(held []domain.Grant, s domain.Scope) domain.Privilege {
	best := domain.NoPrivilege
	for _, g := range held {
		if g.Privilege == domain.Deny {
			if Intersects(g.Scope, s) {
				return domain.NoPrivilege
			}
			continue
		}
		if !Covers(g.Scope, s) {
			continue
		}
		if g.Privilege > best {
			best = g.Privilege
		}
	}
	return best
}

// CanGrant reports whether an actor holding these grants may create a grant of
// privilege p over scope s.
//
// The rule the system we learned from arrived at in 1.8.0, after discovering
// that a System Administrator could add themselves to Global Admins: nobody
// may create a grant conferring a privilege they do not themselves hold over
// that scope. It is enforced in the same function that writes a grant, from
// the first commit, because an escalation check that lives anywhere else is an
// escalation check somebody routes around.
//
// Deny is the one case that is not a comparison. A Deny grant confers nothing,
// so "a privilege they do not hold" does not describe it; what it does is veto
// everyone else across the scope, which is the most powerful thing anybody can
// write there. So writing one requires the top of the positive scale over that
// scope: you may forbid only where you may already do everything.
func CanGrant(held []domain.Grant, s domain.Scope, p domain.Privilege) error {
	if !p.Valid() {
		return fmt.Errorf("privilege %s: not on the scale: %w", p, domain.ErrInvalid)
	}

	have := Delegable(held, s)
	need := p
	if p == domain.Deny {
		need = domain.Publish
	}

	if !have.Allows(need) {
		return fmt.Errorf("%w: granting %s over %s needs %s there, and you hold %s",
			ErrEscalation, p, s, need, have)
	}
	return nil
}
