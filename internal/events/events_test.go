// Copyright (c) 2026 Michael D Henderson.

package events

import "testing"

// TestEveryTypeIsRegistered is the check that replaces the seeded registry of
// 153 rows the system we learned from kept in four tables (DESIGN.md 10).
//
// The registry is a map in Go, which means it can fall out of step with the
// constants beside it in a way a table with a foreign key could not. This is
// the foreign key.
func TestEveryTypeIsRegistered(t *testing.T) {
	for _, eventType := range All() {
		if !Registered(eventType) {
			t.Errorf("%q has no display name", eventType)
		}
		if Name(eventType) == eventType {
			t.Errorf("%q has no display name distinct from its type", eventType)
		}
	}
	if len(All()) != len(names) {
		t.Errorf("All() lists %d types and the registry holds %d; one of them is missing an entry",
			len(All()), len(names))
	}
}

// TestNameNeverReturnsNothing: this ends up in a table a person reads, and an
// unknown type is a row with a blank column rather than an error.
func TestNameNeverReturnsNothing(t *testing.T) {
	if got := Name("something.unregistered"); got != "something.unregistered" {
		t.Errorf("Name of an unknown type = %q", got)
	}
	if Registered("something.unregistered") {
		t.Error("an unknown type reported itself registered")
	}
}

// TestTypesAreDotted is the naming convention, written down where it can fail.
func TestTypesAreDotted(t *testing.T) {
	for _, eventType := range All() {
		dots := 0
		for _, c := range eventType {
			switch {
			case c == '.':
				dots++
			case c >= 'a' && c <= 'z', c == '_':
			default:
				t.Errorf("%q contains %q; event types are lowercase, dotted, and underscored", eventType, c)
			}
		}
		if dots != 1 {
			t.Errorf("%q has %d dots; the shape is subject.verb-in-the-past", eventType, dots)
		}
	}
}
