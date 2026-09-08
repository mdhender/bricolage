// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestPrivilegeNames(t *testing.T) {
	for _, p := range Privileges {
		name := p.String()
		if name == "" || strings.HasPrefix(name, "privilege(") {
			t.Errorf("%d has no name: %q", uint8(p), name)
		}
		if !p.Valid() {
			t.Errorf("%s is not Valid, but it is a level a grant may carry", name)
		}
		got, err := ParsePrivilege(name)
		if err != nil {
			t.Errorf("ParsePrivilege(%q): %v", name, err)
			continue
		}
		if got != p {
			t.Errorf("ParsePrivilege(%q) = %s", name, got)
		}
	}

	if NoPrivilege.Valid() {
		t.Error("NoPrivilege is Valid; it is an answer, not a grant")
	}
	if _, err := ParsePrivilege("none"); !errors.Is(err, ErrInvalid) {
		t.Error("ParsePrivilege accepted \"none\"")
	}
	// A permission that can be spelled two ways is a permission somebody will
	// spell the third way.
	for _, spelling := range []string{"READ", "Read", " read", "publish "} {
		if _, err := ParsePrivilege(spelling); err == nil {
			t.Errorf("ParsePrivilege accepted %q", spelling)
		}
	}
	// A value off the scale still prints, because this ends up in an error
	// message.
	if got := Privilege(200).String(); got != "privilege(200)" {
		t.Errorf("Privilege(200).String() = %q", got)
	}
}

func TestScopeString(t *testing.T) {
	if got := (Scope{}).String(); got != "everything" {
		t.Errorf("the unconstrained scope reads as %q", got)
	}
	if !(Scope{}).IsGlobal() {
		t.Error("the unconstrained scope is not IsGlobal")
	}

	s := Scope{
		SiteID:       Ref(int64(1)),
		DocKind:      Ref("story"),
		CategoryID:   Ref(int64(9)),
		CategoryPath: Ref("/features/"),
		CategoryDeep: true,
	}
	got := s.String()
	for _, want := range []string{"site=1", "kind=story", "category=/features/**"} {
		if !strings.Contains(got, want) {
			t.Errorf("scope reads as %q, want it to contain %q", got, want)
		}
	}
	if s.IsGlobal() {
		t.Error("a constrained scope reported itself global")
	}
}

func TestScopeValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope Scope
		ok    bool
	}{
		{"the unconstrained scope", Scope{}, true},
		{"a site", Scope{SiteID: Ref(int64(1))}, true},
		{"a category with its path", Scope{CategoryID: Ref(int64(9)), CategoryPath: Ref("/features/")}, true},

		{"a category with no path to prefix-test", Scope{CategoryID: Ref(int64(9))}, false},
		{"a category path that does not end in a slash", Scope{CategoryID: Ref(int64(9)), CategoryPath: Ref("/features")}, false},
		{"a path with no category", Scope{CategoryPath: Ref("/features/")}, false},
		{"an empty kind, which is not a wildcard", Scope{DocKind: Ref("")}, false},
		{"an empty state, which is not a wildcard", Scope{State: Ref("")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, want a domain.ErrInvalid", err)
			}
		})
	}
}

func TestGrantValidate(t *testing.T) {
	valid := Grant{RoleID: 1, Privilege: Read}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		grant Grant
	}{
		{"no privilege", Grant{RoleID: 1}},
		{"a privilege off the scale", Grant{RoleID: 1, Privilege: Privilege(9)}},
		{"no role", Grant{Privilege: Read}},
		{"a malformed scope", Grant{RoleID: 1, Privilege: Read, Scope: Scope{CategoryID: Ref(int64(1))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.grant.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want a domain.ErrInvalid", err)
			}
		})
	}
}
