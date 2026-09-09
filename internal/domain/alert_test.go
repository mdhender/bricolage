// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
)

// The pure half of M12: what a condition means, what a rule accepts, and the
// order the three fact sources resolve in. domain is pure, so it is tested
// exhaustively and table-driven (AGENTS.md, "Testing").

// TestConditionOperators walks every operator against values of every shape
// the engine sees. The table is the specification: an operator with no row
// here is an operator nobody has agreed the meaning of.
func TestConditionOperators(t *testing.T) {
	facts := Facts{Payload: map[string]any{
		"to":      "review",
		"version": float64(4), // what encoding/json produces
		"count":   7,          // what a hand-built payload produces
		"title":   "The Budget, Explained",
		"ready":   true,
		"at":      "2026-03-01T00:00:00.000Z",
	}}

	tests := []struct {
		name  string
		cond  Condition
		match bool
	}{
		{"eq on a string", Condition{Field: "to", Op: OpEq, Value: "review"}, true},
		{"eq refuses another string", Condition{Field: "to", Op: OpEq, Value: "draft"}, false},
		{"eq crosses number and string", Condition{Field: "version", Op: OpEq, Value: "4"}, true},
		{"eq on an int payload", Condition{Field: "count", Op: OpEq, Value: float64(7)}, true},
		{"eq on a boolean", Condition{Field: "ready", Op: OpEq, Value: "true"}, true},
		{"ne", Condition{Field: "to", Op: OpNe, Value: "draft"}, true},
		{"ne refuses equality", Condition{Field: "to", Op: OpNe, Value: "review"}, false},
		{"lt numeric", Condition{Field: "version", Op: OpLt, Value: 5}, true},
		{"lt refuses", Condition{Field: "version", Op: OpLt, Value: 4}, false},
		{"lte", Condition{Field: "version", Op: OpLte, Value: 4}, true},
		{"gt", Condition{Field: "version", Op: OpGt, Value: 3}, true},
		{"gte", Condition{Field: "version", Op: OpGte, Value: 4}, true},
		{"lt on a timestamp is lexicographic", Condition{Field: "at", Op: OpLt, Value: "2026-04-01T00:00:00.000Z"}, true},
		{"in", Condition{Field: "to", Op: OpIn, Value: []any{"review", "approved"}}, true},
		{"in refuses", Condition{Field: "to", Op: OpIn, Value: []any{"draft", "archived"}}, false},
		{"in accepts a scalar as a list of one", Condition{Field: "to", Op: OpIn, Value: "review"}, true},
		{"contains", Condition{Field: "title", Op: OpContains, Value: "Budget"}, true},
		{"contains is case sensitive", Condition{Field: "title", Op: OpContains, Value: "budget"}, false},
		{"matches", Condition{Field: "title", Op: OpMatches, Value: `^The .*Explained$`}, true},
		{"matches refuses", Condition{Field: "title", Op: OpMatches, Value: `^Budget`}, false},

		// A field nothing supplies fails every operator, "ne" included. See
		// Condition.Matches: an assertion about a field an event does not
		// carry is an assertion that cannot be made.
		{"an absent field fails eq", Condition{Field: "nope", Op: OpEq, Value: "x"}, false},
		{"an absent field fails ne", Condition{Field: "nope", Op: OpNe, Value: "x"}, false},
		{"an absent field fails matches", Condition{Field: "nope", Op: OpMatches, Value: ".*"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cond.Matches(facts); got != tc.match {
				t.Errorf("%s %s %v against %v = %t, want %t",
					tc.cond.Field, tc.cond.Op, tc.cond.Value, facts.Payload[tc.cond.Field], got, tc.match)
			}
		})
	}
}

// TestEveryOperatorHasABehaviour keeps the vocabulary and the implementation
// together. An operator that parses and then falls through compare's default
// would match nothing, silently, which is the shape of defect invariant 6 is
// about.
func TestEveryOperatorHasABehaviour(t *testing.T) {
	facts := Facts{Payload: map[string]any{"x": "1"}}
	for _, op := range Operators {
		value := any("1")
		if op == OpIn {
			value = []any{"1"}
		}
		if op == OpMatches {
			value = "^1$"
		}
		c := Condition{Field: "x", Op: op, Value: value}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", op, err)
			continue
		}
		// Everything here compares 1 against 1, so the three operators
		// that mean "different" are the three that must refuse it.
		want := op != OpNe && op != OpLt && op != OpGt
		if got := c.Matches(facts); got != want {
			t.Errorf("%s against an equal value = %t, want %t", op, got, want)
		}
	}
}

// TestConditionsAreAnAnd is PLAN.md M12 acceptance 2: a rule with two
// conditions where one fails fires nothing.
func TestConditionsAreAnAnd(t *testing.T) {
	facts := Facts{Payload: map[string]any{"to": "review", "kind": "story"}}

	both := AlertRule{Conditions: []Condition{
		{Field: "to", Op: OpEq, Value: "review"},
		{Field: "kind", Op: OpEq, Value: "story"},
	}}
	if !both.Matches(facts) {
		t.Error("a rule whose conditions all pass did not match")
	}

	one := AlertRule{Conditions: []Condition{
		{Field: "to", Op: OpEq, Value: "review"},
		{Field: "kind", Op: OpEq, Value: "media"},
	}}
	if one.Matches(facts) {
		t.Error("a rule with two conditions, one of which fails, matched; conditions are an AND")
	}

	// A rule with no conditions matches every event of its type. That is the
	// useful degenerate case -- "tell me whenever anything is published" --
	// and not an accident.
	if !(AlertRule{}).Matches(facts) {
		t.Error("a rule with no conditions did not match")
	}
}

// TestFieldResolutionOrder is PLAN.md M12 acceptance 5: a field present on
// both the event payload and the subject resolves from the payload.
func TestFieldResolutionOrder(t *testing.T) {
	facts := Facts{
		Actor:   map[string]any{"actor_uid": "u1", "shared": "actor"},
		Payload: map[string]any{"state": "draft", "shared": "payload"},
		Subject: map[string]any{"state": "review", "shared": "subject", "only_here": "yes"},
	}

	tests := []struct {
		field string
		want  string
	}{
		// The one the acceptance criterion is about: the payload is what was
		// true at the moment the event happened, the subject is what is true
		// now, and an event is a record of a moment.
		{"state", "draft"},
		// The full order, all three carrying the same key.
		{"shared", "actor"},
		// The subject is the fallback for everything the payload did not
		// think to write down.
		{"only_here", "yes"},
		{"actor_uid", "u1"},
	}
	for _, tc := range tests {
		got, ok := facts.Resolve(tc.field)
		if !ok {
			t.Errorf("Resolve(%q) found nothing", tc.field)
			continue
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %v, want %v (actor, then payload, then subject)", tc.field, got, tc.want)
		}
	}

	if _, ok := facts.Resolve("nothing"); ok {
		t.Error("Resolve found a field nothing supplied")
	}

	// The whole vocabulary an event offers, which is what a refusal names.
	if want := "actor_uid,only_here,shared,state"; strings.Join(facts.Fields(), ",") != want {
		t.Errorf("Fields() = %v, want %s", facts.Fields(), want)
	}
}

// TestABrokenPatternIsRefusedAtSaveTime is PLAN.md M12 acceptance 3. The
// message has to name the pattern: the person reading it typed it.
func TestABrokenPatternIsRefusedAtSaveTime(t *testing.T) {
	r := AlertRule{
		Name: "Broken", EventType: "document.transitioned",
		Channel: ChannelInApp, Target: "role:legal",
		Conditions: []Condition{{Field: "title", Op: OpMatches, Value: "([unclosed"}},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("a rule with an uncompilable pattern was accepted")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error is %v; a bad pattern is malformed input, which is ErrInvalid and a 422", err)
	}
	if !strings.Contains(err.Error(), "([unclosed") {
		t.Errorf("error %q does not name the pattern", err)
	}

	// And the same rule with a pattern that compiles is accepted, so the test
	// above cannot pass because Validate refuses everything.
	r.Conditions[0].Value = "^ok"
	if err := r.Validate(); err != nil {
		t.Errorf("a rule with a good pattern was refused: %v", err)
	}
}

// TestCompilePatternCaches asserts the "compiled once and cached" half of
// DESIGN.md 10. Two calls return the same object, so a rule firing a thousand
// times compiles its regexp once.
func TestCompilePatternCaches(t *testing.T) {
	const pattern = `^cached-[0-9]+$`
	first, err := CompilePattern(pattern)
	if err != nil {
		t.Fatalf("CompilePattern: %v", err)
	}
	second, err := CompilePattern(pattern)
	if err != nil {
		t.Fatalf("CompilePattern: %v", err)
	}
	if first != second {
		t.Error("CompilePattern returned a second compilation of the same pattern")
	}
}

// TestAlertRuleValidation walks the refusals a rule can earn.
func TestAlertRuleValidation(t *testing.T) {
	ok := AlertRule{Name: "Fine", EventType: "document.published", Channel: ChannelInApp, Target: "role:legal"}

	tests := []struct {
		name string
		rule AlertRule
		want string
	}{
		{"a good rule", ok, ""},
		{"no name", AlertRule{EventType: "x", Channel: ChannelInApp, Target: "role:a"}, "no name"},
		{"no event type", AlertRule{Name: "n", Channel: ChannelInApp, Target: "role:a"}, "no event type"},
		{"a channel this binary does not deliver",
			AlertRule{Name: "n", EventType: "x", Channel: "carrier-pigeon", Target: "role:a"}, "not a channel"},
		{"a target with no kind",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "legal"}, "want user:UID"},
		{"a target kind that is not one",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "desk:legal"}, "not a target kind"},
		{"an address on the in-app channel",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "email:a@b.example"},
			"only be reached on the email channel"},
		{"an address that is not one",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelEmail, Target: "email:not-an-address"},
			"not an address"},
		{"an operator that is not one",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "role:a",
				Conditions: []Condition{{Field: "f", Op: "approximately", Value: "1"}}}, "not an operator"},
		{"a condition with no field",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "role:a",
				Conditions: []Condition{{Op: OpEq, Value: "1"}}}, "no field"},
		{"in with an empty list",
			AlertRule{Name: "n", EventType: "x", Channel: ChannelInApp, Target: "role:a",
				Conditions: []Condition{{Field: "f", Op: OpIn, Value: []any{}}}}, "non-empty list"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.rule)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error %v does not answer to ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestAlertTargetRoundTrips keeps the three forms parsing back to what they
// were written as. A target is stored as text and typed by a person, so the
// two directions have to agree.
func TestAlertTargetRoundTrips(t *testing.T) {
	tests := []struct {
		target  string
		channel AlertChannel
		kind    string
		name    string
	}{
		{"user:01hx", ChannelInApp, TargetUser, "01hx"},
		{"role:legal", ChannelInApp, TargetRole, "legal"},
		{"email:desk@example.com", ChannelEmail, TargetEmail, "desk@example.com"},
	}
	for _, tc := range tests {
		got, err := ParseAlertTarget(tc.target, tc.channel)
		if err != nil {
			t.Errorf("ParseAlertTarget(%q): %v", tc.target, err)
			continue
		}
		if got.Kind != tc.kind || got.Name != tc.name {
			t.Errorf("ParseAlertTarget(%q) = %+v, want {%s %s}", tc.target, got, tc.kind, tc.name)
		}
		if got.String() != tc.target {
			t.Errorf("String() = %q, want %q", got.String(), tc.target)
		}
	}
}

// TestConditionsJSONRoundTrips keeps the stored form readable by the code that
// wrote it. An empty list is "[]" and never "" or "null", which is what the
// column's DEFAULT says too.
func TestConditionsJSONRoundTrips(t *testing.T) {
	r := AlertRule{Conditions: []Condition{
		{Field: "to", Op: OpEq, Value: "review"},
		{Field: "kind", Op: OpIn, Value: []any{"story", "media"}},
	}}
	encoded, err := r.ConditionsJSON()
	if err != nil {
		t.Fatalf("ConditionsJSON: %v", err)
	}
	back, err := ParseConditions(encoded)
	if err != nil {
		t.Fatalf("ParseConditions: %v", err)
	}
	if len(back) != 2 || back[0].Field != "to" || back[1].Op != OpIn {
		t.Fatalf("round trip produced %+v", back)
	}
	// The list survives as a list, which is what "in" needs.
	if got := back[1].Matches(Facts{Payload: map[string]any{"kind": "media"}}); !got {
		t.Error("an \"in\" condition stopped matching after a round trip")
	}

	empty, err := (AlertRule{}).ConditionsJSON()
	if err != nil || empty != "[]" {
		t.Errorf("ConditionsJSON of no conditions = %q, %v; want \"[]\"", empty, err)
	}
	for _, s := range []string{"", "[]", "null"} {
		got, err := ParseConditions(s)
		if err != nil || got != nil {
			t.Errorf("ParseConditions(%q) = %v, %v; want no conditions", s, got, err)
		}
	}
}

// TestAlertBatchNext keeps the cursor arithmetic honest: an empty batch does
// not move it, and a full one moves it to its last row.
func TestAlertBatchNext(t *testing.T) {
	if got := (AlertBatch{Cursor: 7}).Next(); got != 7 {
		t.Errorf("an empty batch moved the cursor to %d", got)
	}
	b := AlertBatch{Cursor: 7, Events: []Event{{ID: 8}, {ID: 9}, {ID: 12}}}
	if got := b.Next(); got != 12 {
		t.Errorf("Next() = %d, want 12", got)
	}
}
