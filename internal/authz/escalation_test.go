// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"errors"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// TestCanGrant is the anti-escalation rule (DESIGN.md 7.3, invariant 12) as a
// table.
//
// The case PLAN.md M2 acceptance 7 names is "edit holder may not grant
// publish". The rest of the table is the reason the check is scope containment
// rather than a comparison against one subject: a grant over one site says
// nothing about a scope that spans every site, and the version of this check
// that gets written by accident lets a site editor write a global grant.
func TestCanGrant(t *testing.T) {
	site1 := domain.Scope{SiteID: ref(int64(1))}
	site2 := domain.Scope{SiteID: ref(int64(2))}
	global := domain.Scope{}
	film := domain.Scope{
		CategoryID:   ref(int64(9)),
		CategoryPath: ref("/features/film/"),
		CategoryDeep: true,
	}
	features := domain.Scope{
		CategoryID:   ref(int64(8)),
		CategoryPath: ref("/features/"),
		CategoryDeep: true,
	}

	for _, tc := range []struct {
		name  string
		held  []domain.Grant
		scope domain.Scope
		want  domain.Privilege
		ok    bool
	}{
		{
			name:  "edit may not grant publish",
			held:  []domain.Grant{grant(domain.Edit, global)},
			scope: global,
			want:  domain.Publish,
			ok:    false,
		},
		{
			name:  "edit may grant edit",
			held:  []domain.Grant{grant(domain.Edit, global)},
			scope: global,
			want:  domain.Edit,
			ok:    true,
		},
		{
			name:  "edit may grant read, because the scale is cumulative",
			held:  []domain.Grant{grant(domain.Edit, global)},
			scope: global,
			want:  domain.Read,
			ok:    true,
		},
		{
			name:  "nothing held grants nothing",
			scope: global,
			want:  domain.Read,
			ok:    false,
		},
		{
			name:  "publish on one site may not be granted globally",
			held:  []domain.Grant{grant(domain.Publish, site1)},
			scope: global,
			want:  domain.Read,
			ok:    false,
		},
		{
			name:  "publish on one site may be granted on that site",
			held:  []domain.Grant{grant(domain.Publish, site1)},
			scope: site1,
			want:  domain.Publish,
			ok:    true,
		},
		{
			name:  "publish on one site may not be granted on another",
			held:  []domain.Grant{grant(domain.Publish, site1)},
			scope: site2,
			want:  domain.Read,
			ok:    false,
		},
		{
			name:  "a global holder may grant on a narrower scope",
			held:  []domain.Grant{grant(domain.Publish, global)},
			scope: site1,
			want:  domain.Publish,
			ok:    true,
		},
		{
			name:  "a deep category covers a descendant scope",
			held:  []domain.Grant{grant(domain.Create, features)},
			scope: film,
			want:  domain.Create,
			ok:    true,
		},
		{
			name:  "a descendant does not cover its ancestor",
			held:  []domain.Grant{grant(domain.Create, film)},
			scope: features,
			want:  domain.Read,
			ok:    false,
		},
		{
			name: "a deny touching the scope takes it all away",
			held: []domain.Grant{
				grant(domain.Publish, global),
				grant(domain.Deny, site1),
			},
			scope: global,
			want:  domain.Read,
			ok:    false,
		},
		{
			name: "a deny elsewhere leaves the scope alone",
			held: []domain.Grant{
				grant(domain.Publish, site1),
				grant(domain.Deny, site2),
			},
			scope: site1,
			want:  domain.Publish,
			ok:    true,
		},
		{
			name:  "writing a deny needs the top of the positive scale there",
			held:  []domain.Grant{grant(domain.Create, global)},
			scope: global,
			want:  domain.Deny,
			ok:    false,
		},
		{
			name:  "publish may write a deny",
			held:  []domain.Grant{grant(domain.Publish, global)},
			scope: global,
			want:  domain.Deny,
			ok:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CanGrant(tc.held, tc.scope, tc.want)
			if tc.ok && err != nil {
				t.Fatalf("CanGrant refused: %v", err)
			}
			if tc.ok {
				return
			}
			if err == nil {
				t.Fatalf("CanGrant allowed %s over %s", tc.want, tc.scope)
			}
			// The refusal must be a forbidden, so that the transport edge maps
			// it to 403 through the one mapping function it has.
			if !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("CanGrant returned %v, which is not a domain.ErrForbidden", err)
			}
		})
	}
}

// TestCanGrantRefusesAPrivilegeThatIsNotOne is the malformed-input half: a
// privilege off the scale is a 422, not a 403.
func TestCanGrantRefusesAPrivilegeThatIsNotOne(t *testing.T) {
	held := []domain.Grant{grant(domain.Publish, domain.Scope{})}
	err := CanGrant(held, domain.Scope{}, domain.Privilege(9))
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("CanGrant returned %v, want a domain.ErrInvalid", err)
	}
}

// TestNobodyEscalatesThroughARoleTheyHold is the shape of the defect the rule
// was added for, in 1.8.0 of the system we learned from: a System
// Administrator could add themselves to Global Admins. Holding the highest
// privilege over a narrow scope must not confer the right to widen it.
func TestNobodyEscalatesThroughANarrowGrant(t *testing.T) {
	siteAdmin := []domain.Grant{grant(domain.Publish, domain.Scope{SiteID: ref(int64(1))})}

	for _, p := range domain.Privileges {
		if err := CanGrant(siteAdmin, domain.Scope{}, p); err == nil {
			t.Errorf("a site administrator was allowed to grant %s over everything", p)
		}
	}
}
