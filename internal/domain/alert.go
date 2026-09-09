// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Alert rules and the notifications they produce (DESIGN.md 10, PLAN.md M12).
//
// A rule is a question asked of every event of one type: does this event
// satisfy all of these conditions, and if it does, who should be told. Both
// halves are pure functions over values, which is why they are here: the
// dispatcher reads the rows, this decides, and internal/store writes what it
// is told.
//
// Deliberately absent: a scripting language. The system we learned from
// evaluated alert rules inside a "Safe" sandbox -- a security surface for
// something a switch statement handles -- and the cost of that choice was that
// nobody could say what a rule was able to do. Here the operators are a closed
// set, every one of them is implemented below, and a rule that names anything
// else is refused when it is written (invariant 6).

// SubjectAlertRule is the events subject kind for an alert rule. A
// notification is not a subject of its own: it is what a rule did about
// somebody else's event, and its history is that event's.
const SubjectAlertRule = "alert_rule"

// Operator is the comparison a condition performs (DESIGN.md 10).
type Operator string

// The operators. This is the whole vocabulary; ParseOperator refuses anything
// else, and every one of them has a case in compare below.
const (
	OpEq       Operator = "eq"
	OpNe       Operator = "ne"
	OpLt       Operator = "lt"
	OpLte      Operator = "lte"
	OpGt       Operator = "gt"
	OpGte      Operator = "gte"
	OpIn       Operator = "in"
	OpContains Operator = "contains"
	OpMatches  Operator = "matches"
)

// Operators are the comparisons a condition may carry, in a stable order for
// the CLI and the admin screens.
var Operators = []Operator{OpEq, OpNe, OpLt, OpLte, OpGt, OpGte, OpIn, OpContains, OpMatches}

// ParseOperator maps a name to an operator. It accepts exactly the names above
// and nothing else: an operator that can be spelled two ways is an operator
// somebody will spell the third way.
func ParseOperator(s string) (Operator, error) {
	for _, op := range Operators {
		if string(op) == s {
			return op, nil
		}
	}
	return "", fmt.Errorf("%q: not an operator (%s): %w",
		s, strings.Join(operatorNames(), ", "), ErrInvalid)
}

func operatorNames() []string {
	out := make([]string, 0, len(Operators))
	for _, op := range Operators {
		out = append(out, string(op))
	}
	return out
}

// AlertChannel is where a notification goes.
type AlertChannel string

const (
	// ChannelInApp writes a notifications row, which is what
	// GET /api/v1/notifications reads. It is delivered inside the same
	// transaction that advances the dispatcher's cursor, because it is a row
	// in this database and nothing about it can fail halfway.
	ChannelInApp AlertChannel = "in_app"

	// ChannelEmail leaves the process, so it is delivered after that
	// transaction commits and behind an interface with a logging
	// implementation as the default (PLAN.md M12). A channel that failed
	// while it still had a transaction open would be a channel that could
	// roll back an editorial action, which is the one thing DESIGN.md 6.4
	// says alerts must never do.
	ChannelEmail AlertChannel = "email"
)

// AlertChannels are the channels a rule may name, in a stable order.
var AlertChannels = []AlertChannel{ChannelInApp, ChannelEmail}

// ParseAlertChannel maps a name to a channel.
func ParseAlertChannel(s string) (AlertChannel, error) {
	for _, c := range AlertChannels {
		if string(c) == s {
			return c, nil
		}
	}
	return "", fmt.Errorf("%q: not a channel (in_app, email): %w", s, ErrInvalid)
}

// The three target forms. A target says who is told, and it says so in a shape
// that names exactly one of them: "user:01j...", "role:editor",
// "email:desk@example.com".
//
// The prefix is required rather than inferred. A bare string would have to be
// guessed at -- is "editor" a role or somebody's uid -- and a rule that
// notified the wrong audience because a guess went the other way is a rule
// whose failure nobody notices, because it looks exactly like a rule that
// matched nothing.
const (
	TargetUser  = "user"
	TargetRole  = "role"
	TargetEmail = "email"
)

// AlertTarget is a parsed target: which kind, and the identifier after the
// colon.
type AlertTarget struct {
	Kind string
	Name string
}

// String renders the target the way it is stored and typed.
func (t AlertTarget) String() string { return t.Kind + ":" + t.Name }

// ParseAlertTarget splits a target into its kind and its name.
//
// "email:" is refused on the in-app channel, because an address is not
// somebody this database can put a row against; a rule that wanted to mail an
// outside address and quietly wrote nothing would be a rule that reported
// success for doing nothing.
func ParseAlertTarget(s string, channel AlertChannel) (AlertTarget, error) {
	kind, name, ok := strings.Cut(s, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return AlertTarget{}, fmt.Errorf(
			"target %q: want user:UID, role:SLUG, or email:ADDRESS: %w", s, ErrInvalid)
	}
	switch kind {
	case TargetUser, TargetRole:
		return AlertTarget{Kind: kind, Name: name}, nil
	case TargetEmail:
		if channel != ChannelEmail {
			return AlertTarget{}, fmt.Errorf(
				"target %q: an address can only be reached on the email channel: %w", s, ErrInvalid)
		}
		if err := ValidateEmail(name); err != nil {
			return AlertTarget{}, err
		}
		return AlertTarget{Kind: kind, Name: name}, nil
	default:
		return AlertTarget{}, fmt.Errorf(
			"target %q: %q is not a target kind (user, role, email): %w", s, kind, ErrInvalid)
	}
}

// Condition is one test a rule applies to an event (DESIGN.md 10).
//
// Value is whatever the JSON carried: a string, a number, a boolean, or -- for
// "in" -- an array of them. It is compared against the resolved field by
// compare below, which coerces rather than insisting on a type: a rule saying
// version gt 3 should not have to know whether the payload wrote 3 or "3".
type Condition struct {
	Field string   `json:"field"`
	Op    Operator `json:"op"`
	Value any      `json:"value"`
}

// Validate reports whether the condition is one the engine can evaluate.
//
// A "matches" pattern is compiled here, which is the whole of PLAN.md M12
// acceptance 3: a rule whose regexp does not compile is refused when it is
// saved, with the error the person writing it can act on, rather than at fire
// time -- where the only person who would ever see it is whoever reads the
// server log, about an event that has already happened.
func (c Condition) Validate() error {
	if strings.TrimSpace(c.Field) == "" {
		return fmt.Errorf("condition: no field: %w", ErrInvalid)
	}
	if _, err := ParseOperator(string(c.Op)); err != nil {
		return fmt.Errorf("condition on %s: %w", c.Field, err)
	}
	switch c.Op {
	case OpIn:
		if len(conditionList(c.Value)) == 0 {
			return fmt.Errorf(
				"condition on %s: \"in\" wants a non-empty list of values: %w", c.Field, ErrInvalid)
		}
	case OpMatches:
		pattern, ok := c.Value.(string)
		if !ok {
			return fmt.Errorf(
				"condition on %s: \"matches\" wants a pattern, not %T: %w", c.Field, c.Value, ErrInvalid)
		}
		if _, err := CompilePattern(pattern); err != nil {
			return fmt.Errorf("condition on %s: %w", c.Field, err)
		}
	}
	return nil
}

// patterns caches compiled regexps by their source (DESIGN.md 10, "compiled
// once and cached").
//
// It is bounded by the number of distinct patterns in the alert_rules table,
// which is bounded by the number of rules somebody has written, because the
// only way into this cache is a rule that was saved and every rule is
// validated before it is saved.
var patterns sync.Map

// CompilePattern compiles a "matches" pattern, or returns the compilation it
// already has. The error names the pattern, because that is what the person
// who typed it needs to see.
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	if hit, ok := patterns.Load(pattern); ok {
		return hit.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern %q does not compile: %v: %w", pattern, err, ErrInvalid)
	}
	patterns.Store(pattern, re)
	return re, nil
}

// AlertRule is one rule over the event stream.
type AlertRule struct {
	ID  int64
	UID string

	Name      string
	EventType string

	Conditions []Condition

	Channel AlertChannel
	Target  string

	Active bool

	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ParsedTarget returns the rule's target, split. It has been validated by the
// time a rule is in the database, so a rule read back always parses.
func (r AlertRule) ParsedTarget() (AlertTarget, error) {
	return ParseAlertTarget(r.Target, r.Channel)
}

// Validate reports whether the rule is one this binary can run.
//
// Everything it checks is checked before the rule is written, and nothing is
// checked at fire time: an alert that turns out to be unrunnable an hour after
// it was saved is an alert nobody is watching for.
//
// It does not check that EventType is a type this binary knows. That is
// internal/events' registry, and domain imports nothing (invariant 1); the
// service asks the registry, which is where the check belongs and where its
// error can say which types exist.
func (r AlertRule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("alert rule: no name: %w", ErrInvalid)
	}
	if strings.TrimSpace(r.EventType) == "" {
		return fmt.Errorf("alert rule %q: no event type: %w", r.Name, ErrInvalid)
	}
	if _, err := ParseAlertChannel(string(r.Channel)); err != nil {
		return fmt.Errorf("alert rule %q: %w", r.Name, err)
	}
	if _, err := ParseAlertTarget(r.Target, r.Channel); err != nil {
		return fmt.Errorf("alert rule %q: %w", r.Name, err)
	}
	for _, c := range r.Conditions {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("alert rule %q: %w", r.Name, err)
		}
	}
	return nil
}

// ConditionsJSON renders the conditions for storage. An empty list is "[]",
// never "" and never "null", which is what the column's DEFAULT says too.
func (r AlertRule) ConditionsJSON() (string, error) {
	if len(r.Conditions) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(r.Conditions)
	if err != nil {
		return "", fmt.Errorf("alert rule %q: encoding the conditions: %w", r.Name, err)
	}
	return string(b), nil
}

// ParseConditions reads the stored JSON back.
func ParseConditions(s string) ([]Condition, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "null" {
		return nil, nil
	}
	var out []Condition
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("conditions %q: %w", s, err)
	}
	return out, nil
}

// Facts are the three sources a condition's field is resolved from
// (DESIGN.md 10).
//
// The order is the design's and it is load-bearing: the acting user, then the
// event payload, then the subject. A field present on more than one resolves
// from the first, which means a rule about "state" on a document.created event
// is asking what the state was when the document was created and not what it
// is now -- the event is a record of a moment, and the moment is what the
// payload carries. The subject is the fallback, for everything the payload did
// not think to write down.
type Facts struct {
	Actor   map[string]any
	Payload map[string]any
	Subject map[string]any
}

// Resolve returns the value of a field and whether anything supplied one.
func (f Facts) Resolve(field string) (any, bool) {
	for _, source := range []map[string]any{f.Actor, f.Payload, f.Subject} {
		if v, ok := source[field]; ok {
			return v, true
		}
	}
	return nil, false
}

// Fields returns every field name these facts can resolve, sorted. It is what
// a refusal and a "why did this rule not match" listing name.
func (f Facts) Fields() []string {
	seen := map[string]struct{}{}
	for _, source := range []map[string]any{f.Actor, f.Payload, f.Subject} {
		for k := range source {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Matches reports whether every condition passes (PLAN.md M12 acceptance 2).
//
// All of them, or none of it. A rule is an AND of its conditions and there is
// no OR: "or" is two rules, which cost one row each and can be turned off
// separately.
//
// A rule with no conditions matches every event of its type, which is the
// useful degenerate case -- "tell me whenever anything is published" -- and
// not an accident.
func (r AlertRule) Matches(f Facts) bool {
	for _, c := range r.Conditions {
		if !c.Matches(f) {
			return false
		}
	}
	return true
}

// Matches reports whether one condition passes.
//
// A field nothing supplies fails every operator, "ne" included. A condition is
// an assertion about a field, and an event that does not carry the field is an
// event the assertion cannot be made about; answering "ne" with true would
// have every rule fire on every event that happens not to mention the column
// it is about, which is the noisiest possible reading of the quietest possible
// fact.
func (c Condition) Matches(f Facts) bool {
	value, ok := f.Resolve(c.Field)
	if !ok {
		return false
	}
	return compare(value, c)
}

// compare applies one operator. It is the closed set the schema's comment
// promises: every operator has a case, and there is no default that guesses.
func compare(value any, c Condition) bool {
	switch c.Op {
	case OpEq:
		return equal(value, c.Value)
	case OpNe:
		return !equal(value, c.Value)
	case OpLt, OpLte, OpGt, OpGte:
		cmp, ok := order(value, c.Value)
		if !ok {
			return false
		}
		switch c.Op {
		case OpLt:
			return cmp < 0
		case OpLte:
			return cmp <= 0
		case OpGt:
			return cmp > 0
		default:
			return cmp >= 0
		}
	case OpIn:
		for _, want := range conditionList(c.Value) {
			if equal(value, want) {
				return true
			}
		}
		return false
	case OpContains:
		return strings.Contains(text(value), text(c.Value))
	case OpMatches:
		pattern, ok := c.Value.(string)
		if !ok {
			return false
		}
		re, err := CompilePattern(pattern)
		if err != nil {
			// Unreachable through a saved rule: Validate compiled this
			// pattern before the row was written. A rule edited into the
			// database by hand matches nothing rather than panicking.
			return false
		}
		return re.MatchString(text(value))
	default:
		return false
	}
}

// equal compares two values the way a rule means it.
//
// Numerically when both sides are numbers, so that a payload writing 3 and a
// rule saying "3" agree; by text otherwise, so that a state name, a uid, and a
// timestamp all compare the way they read. A boolean compares as "true" or
// "false", which is what a person types.
func equal(a, b any) bool {
	if x, y, ok := numbers(a, b); ok {
		return x == y
	}
	return text(a) == text(b)
}

// order compares two values, reporting false when they cannot be ordered.
//
// Timestamps order correctly as text because this system writes them fixed
// width, in UTC, with a trailing Z (DESIGN.md 13.5) -- the same property that
// lets SQL compare them without a function call.
//
// An empty string on either side is not ordered against. A field that resolved
// to "" is a field with nothing in it, and every ordering operator against it
// would otherwise be answering a question about the empty string rather than
// about the value somebody meant -- "slug gt ..." on a document with no slug
// would be true or false for reasons nobody writing the rule intended.
func order(a, b any) (int, bool) {
	if x, y, ok := numbers(a, b); ok {
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		default:
			return 0, true
		}
	}
	x, y := text(a), text(b)
	if x == "" || y == "" {
		return 0, false
	}
	return strings.Compare(x, y), true
}

// numbers reports both values as float64 when both are numeric. A JSON number
// arrives as float64 and a hand-built payload may carry an int, so both are
// accepted, and so is a numeric string -- a rule is typed by a person into a
// text field.
func numbers(a, b any) (float64, float64, bool) {
	x, ok := number(a)
	if !ok {
		return 0, 0, false
	}
	y, ok := number(b)
	if !ok {
		return 0, 0, false
	}
	return x, y, true
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil && strings.TrimSpace(n) != ""
	default:
		return 0, false
	}
}

// text renders a value the way a rule compares it. A float that is a whole
// number renders without a decimal point, because JSON gives every number as a
// float64 and "version eq 3" must not have to be written "3.0".
func text(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case bool:
		return strconv.FormatBool(s)
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(s), 'f', -1, 32)
	case int:
		return strconv.Itoa(s)
	case int32:
		return strconv.FormatInt(int64(s), 10)
	case int64:
		return strconv.FormatInt(s, 10)
	case time.Time:
		return s.UTC().Format(time.RFC3339Nano)
	case json.Number:
		return s.String()
	default:
		return fmt.Sprint(v)
	}
}

// conditionList reads the operand of "in" as a list. A scalar is a list of
// one, because "in" with a single value is what somebody writes on the way to
// adding the second.
func conditionList(v any) []any {
	switch list := v.(type) {
	case nil:
		return nil
	case []any:
		return list
	case []string:
		out := make([]any, 0, len(list))
		for _, s := range list {
			out = append(out, s)
		}
		return out
	default:
		return []any{v}
	}
}

// AlertBatch is a run of events waiting to be evaluated, and the cursor they
// were read at.
//
// The cursor travels with the events because delivering a batch is a
// compare-and-swap: the store writes the notifications and moves the cursor in
// one transaction, naming the value the batch was read at, so two dispatchers
// cannot both conclude they delivered it.
type AlertBatch struct {
	Cursor int64
	Events []Event
}

// Next is the cursor position after this batch, which is the id of the last
// event in it. An empty batch does not move it.
func (b AlertBatch) Next() int64 {
	if len(b.Events) == 0 {
		return b.Cursor
	}
	return b.Events[len(b.Events)-1].ID
}

// NewNotification is one row a delivery writes.
type NewNotification struct {
	UID     string
	UserID  int64
	EventID int64

	// RuleID is the rule that produced it. It is never 0 on the way in: a
	// notification with no rule behind it is one whose rule was deleted
	// afterwards, which notifications.rule_id's ON DELETE SET NULL produces
	// and nothing else may.
	RuleID    int64
	CreatedAt time.Time
}

// Notification is one thing somebody has been told (DESIGN.md 10).
//
// The event's type, subject, and payload travel with it because a notification
// with no news in it is a row that makes a person go and look something up. It
// is the store that joins them; nothing here reads anything.
type Notification struct {
	ID  int64
	UID string

	UserID  int64
	EventID int64

	// RuleID is the rule that produced it, or 0 when that rule has since been
	// deleted. The row survives its rule: what it says happened still
	// happened. RuleUID and RuleName are joined in by the store, so that the
	// API can name the rule without a second query and without ever speaking
	// the integer (invariant 10).
	RuleID   int64
	RuleUID  string
	RuleName string

	ReadAt    time.Time
	CreatedAt time.Time

	EventType   string
	SubjectKind string
	SubjectID   int64
	Payload     map[string]any
	OccurredAt  time.Time
}

// Read reports whether it has been read. "Unread" is the absence of a
// timestamp, which is why there is no status column.
func (n Notification) Read() bool { return !n.ReadAt.IsZero() }
