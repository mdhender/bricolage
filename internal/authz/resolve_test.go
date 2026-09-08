// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// ref is domain.Ref, spelled shorter, because a scope literal in a table is
// mostly pointers.
func ref[T any](v T) *T { return domain.Ref(v) }

// grant builds a grant carrying one privilege over one scope. The role and the
// timestamps do not matter to resolution, which is the point of resolution
// being pure.
func grant(p domain.Privilege, s domain.Scope) domain.Grant {
	return domain.Grant{RoleID: 1, Privilege: p, Scope: s}
}

// story is the subject most cases resolve against: a story in /features/film/,
// on site 1, in the review state of workflow 7.
var story = domain.Subject{
	SiteID:       1,
	DocKind:      "story",
	CategoryPath: "/features/film/",
	WorkflowID:   7,
	State:        "review",
	CollectionID: 3,
	DocumentID:   42,
}

// TestResolve is PLAN.md M2 acceptance 6.
//
// Every case in the acceptance criterion is here: no grants, two grants, a
// DENY anywhere, category_deep matching a descendant and not matching without
// it, and every NULL column behaving as a wildcard.
func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []domain.Grant
		subj   domain.Subject
		want   domain.Privilege
	}{
		{
			name: "no grants resolve to nothing",
			subj: story,
			want: domain.NoPrivilege,
		},
		{
			name:   "the global grant is a wildcard on every column",
			grants: []domain.Grant{grant(domain.Edit, domain.Scope{})},
			subj:   story,
			want:   domain.Edit,
		},
		{
			name: "two grants resolve to the maximum",
			grants: []domain.Grant{
				grant(domain.Read, domain.Scope{}),
				grant(domain.Create, domain.Scope{}),
			},
			subj: story,
			want: domain.Create,
		},
		{
			name: "the maximum ignores the order they are loaded in",
			grants: []domain.Grant{
				grant(domain.Create, domain.Scope{}),
				grant(domain.Read, domain.Scope{}),
			},
			subj: story,
			want: domain.Create,
		},
		{
			name: "a deny anywhere wins",
			grants: []domain.Grant{
				grant(domain.Publish, domain.Scope{}),
				grant(domain.Deny, domain.Scope{SiteID: ref(int64(1))}),
			},
			subj: story,
			want: domain.Deny,
		},
		{
			name: "a deny on a scope that does not match does not apply",
			grants: []domain.Grant{
				grant(domain.Publish, domain.Scope{}),
				grant(domain.Deny, domain.Scope{SiteID: ref(int64(2))}),
			},
			subj: story,
			want: domain.Publish,
		},
		{
			name: "category_deep matches a descendant",
			grants: []domain.Grant{grant(domain.Edit, domain.Scope{
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/features/"),
				CategoryDeep: true,
			})},
			subj: story,
			want: domain.Edit,
		},
		{
			name: "without category_deep a descendant does not match",
			grants: []domain.Grant{grant(domain.Edit, domain.Scope{
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/features/"),
				CategoryDeep: false,
			})},
			subj: story,
			want: domain.NoPrivilege,
		},
		{
			name: "without category_deep the category itself still matches",
			grants: []domain.Grant{grant(domain.Edit, domain.Scope{
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/features/film/"),
				CategoryDeep: false,
			})},
			subj: story,
			want: domain.Edit,
		},
		{
			name: "a deep category does not match a sibling with a shared prefix",
			grants: []domain.Grant{grant(domain.Edit, domain.Scope{
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/feat/"),
				CategoryDeep: true,
			})},
			subj: story,
			want: domain.NoPrivilege,
		},
		{
			name:   "a site constraint that matches",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{SiteID: ref(int64(1))})},
			subj:   story,
			want:   domain.Read,
		},
		{
			name:   "a site constraint that does not",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{SiteID: ref(int64(2))})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name:   "a document kind constraint that does not match",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{DocKind: ref("media")})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name:   "a workflow constraint that does not match",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{WorkflowID: ref(int64(8))})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name:   "a state constraint that does not match",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{State: ref("draft")})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name:   "a collection constraint that does not match",
			grants: []domain.Grant{grant(domain.Read, domain.Scope{CollectionID: ref(int64(4))})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name:   "a per-document grant",
			grants: []domain.Grant{grant(domain.Publish, domain.Scope{DocumentID: ref(int64(42))})},
			subj:   story,
			want:   domain.Publish,
		},
		{
			name:   "a per-document grant on another document",
			grants: []domain.Grant{grant(domain.Publish, domain.Scope{DocumentID: ref(int64(43))})},
			subj:   story,
			want:   domain.NoPrivilege,
		},
		{
			name: "every column stated at once, all matching",
			grants: []domain.Grant{grant(domain.Recall, domain.Scope{
				SiteID:       ref(int64(1)),
				DocKind:      ref("story"),
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/features/"),
				CategoryDeep: true,
				WorkflowID:   ref(int64(7)),
				State:        ref("review"),
				CollectionID: ref(int64(3)),
				DocumentID:   ref(int64(42)),
			})},
			subj: story,
			want: domain.Recall,
		},
		{
			name: "every column stated at once, one of them wrong",
			grants: []domain.Grant{grant(domain.Recall, domain.Scope{
				SiteID:       ref(int64(1)),
				DocKind:      ref("story"),
				CategoryID:   ref(int64(9)),
				CategoryPath: ref("/features/"),
				CategoryDeep: true,
				WorkflowID:   ref(int64(7)),
				State:        ref("draft"),
				CollectionID: ref(int64(3)),
				DocumentID:   ref(int64(42)),
			})},
			subj: story,
			want: domain.NoPrivilege,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.grants, tc.subj); got != tc.want {
				t.Errorf("Resolve = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDenyIsNotPermission is the trap the numeric maximum sets.
//
// Deny is 255 so that MAX resolves it with no second pass. That same 255 would
// satisfy every ">= want" comparison, so a caller that compares directly turns
// a veto into universal permission. Allows is the one place that knows, and
// this is the test that says so.
func TestDenyIsNotPermission(t *testing.T) {
	grants := []domain.Grant{grant(domain.Deny, domain.Scope{})}

	if got := Resolve(grants, story); got != domain.Deny {
		t.Fatalf("Resolve = %s, want deny", got)
	}
	for _, want := range domain.Privileges {
		if want == domain.Deny {
			continue
		}
		if Allows(grants, story, want) {
			t.Errorf("a denied subject was allowed %s", want)
		}
	}
	// And the comparison that would have gone wrong.
	if !(domain.Deny >= domain.Publish) {
		t.Fatal("deny is no longer the numeric maximum; MAX resolution depends on it")
	}
}

// TestPrivilegeIsOrderedAndCumulative is the other property DESIGN.md 7
// requires: "may they do this?" is one integer comparison, so a higher level
// permits everything below it.
func TestPrivilegeIsOrderedAndCumulative(t *testing.T) {
	scale := []domain.Privilege{domain.Read, domain.Edit, domain.Recall, domain.Create, domain.Publish}
	for i, held := range scale {
		for j, want := range scale {
			got := held.Allows(want)
			if expect := i >= j; got != expect {
				t.Errorf("%s.Allows(%s) = %t, want %t", held, want, got, expect)
			}
		}
	}
}
