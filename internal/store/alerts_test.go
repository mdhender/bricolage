// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
)

// The store half of M12, against a real in-memory database with foreign keys
// on (invariant 22). What is under test is the two properties the SQL carries
// rather than the engine above it: the cursor and the notifications it
// acknowledges move together, and a compare-and-swap is what stops two
// dispatchers delivering the same batch.

// alertFixture is a database with a document to write events against, so that
// the events the dispatcher would read are real rows written the ordinary way.
type alertFixture struct {
	*docFixture
	rule domain.AlertRule
}

func newAlertFixture(t *testing.T) *alertFixture {
	t.Helper()
	f := &alertFixture{docFixture: newDocFixture(t)}

	rule, err := f.db.CreateAlertRule(t.Context(), domain.AlertRule{
		UID:       ids.MustNew(f.now),
		Name:      "Everything",
		EventType: events.DocumentCreated,
		Channel:   domain.ChannelInApp,
		Target:    "user:" + f.author.UID,
		Active:    true,
		CreatedBy: f.author.ID,
		CreatedAt: f.now,
		UpdatedAt: f.now,
	}, f.event(f.author.ID, events.AlertRuleCreated))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	f.rule = rule
	return f
}

// TestAlertRuleRoundTrips keeps a rule readable by the code that wrote it,
// conditions included: they go into a TEXT column as JSON and come back as
// values the engine evaluates.
func TestAlertRuleRoundTrips(t *testing.T) {
	f := newAlertFixture(t)

	written, err := f.db.CreateAlertRule(t.Context(), domain.AlertRule{
		UID:       ids.MustNew(f.now),
		Name:      "Into review",
		EventType: events.DocumentTransitioned,
		Conditions: []domain.Condition{
			{Field: "to", Op: domain.OpEq, Value: "review"},
			{Field: "kind", Op: domain.OpIn, Value: []any{"story", "media"}},
		},
		Channel:   domain.ChannelInApp,
		Target:    "role:editor",
		Active:    true,
		CreatedBy: f.author.ID,
		CreatedAt: f.now,
		UpdatedAt: f.now,
	}, f.event(f.author.ID, events.AlertRuleCreated))
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}

	read, err := f.db.AlertRuleByUID(t.Context(), written.UID)
	if err != nil {
		t.Fatalf("AlertRuleByUID: %v", err)
	}
	if len(read.Conditions) != 2 {
		t.Fatalf("read back %d conditions, want 2", len(read.Conditions))
	}
	if !read.Matches(domain.Facts{Payload: map[string]any{"to": "review", "kind": "story"}}) {
		t.Error("a rule read back from the database stopped matching what it matched on the way in")
	}
	if read.CreatedBy != f.author.ID {
		t.Errorf("created_by = %d, want %d", read.CreatedBy, f.author.ID)
	}

	// The event is written in the same transaction as the row (invariant 7).
	written2, err := f.db.EventsOfType(t.Context(), events.AlertRuleCreated, 10)
	if err != nil {
		t.Fatalf("EventsOfType: %v", err)
	}
	if len(written2) != 2 { // this one and the fixture's
		t.Fatalf("%d alert_rule.created events, want 2", len(written2))
	}
	if written2[0].SubjectKind != domain.SubjectAlertRule {
		t.Errorf("subject kind = %q, want %q", written2[0].SubjectKind, domain.SubjectAlertRule)
	}

	// Only the active ones reach the dispatcher.
	off := read
	off.Active = false
	if _, err := f.db.UpdateAlertRule(t.Context(), off, f.event(f.author.ID, events.AlertRuleUpdated)); err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	active, err := f.db.ActiveAlertRules(t.Context())
	if err != nil {
		t.Fatalf("ActiveAlertRules: %v", err)
	}
	for _, r := range active {
		if r.UID == read.UID {
			t.Error("a rule switched off is still being evaluated")
		}
	}
	all, err := f.db.ListAlertRules(t.Context())
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListAlertRules returned %d; a rule that is off is still configuration somebody has to see", len(all))
	}
}

// TestDeletingARuleKeepsItsNotifications is the ON DELETE SET NULL of
// migration 0012, which is the one clause in that table DESIGN.md 10 spells
// out: what a rule already told somebody still happened.
func TestDeletingARuleKeepsItsNotifications(t *testing.T) {
	f := newAlertFixture(t)
	f.create(t, "Something Happened")

	batch, err := f.db.PendingAlertEvents(t.Context(), 10)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	if len(batch.Events) == 0 {
		t.Fatal("no events are waiting; the fixture wrote some")
	}
	written, moved, err := f.db.Deliver(t.Context(), batch.Cursor, batch.Next(), []domain.NewNotification{{
		UID: ids.MustNew(f.now), UserID: f.author.ID, EventID: batch.Events[0].ID,
		RuleID: f.rule.ID, CreatedAt: f.now,
	}}, f.now)
	if err != nil || !moved || written != 1 {
		t.Fatalf("Deliver = (%d, %t, %v)", written, moved, err)
	}

	if err := f.db.DeleteAlertRule(t.Context(), f.rule.ID, f.event(f.author.ID, events.AlertRuleDeleted)); err != nil {
		t.Fatalf("DeleteAlertRule: %v", err)
	}

	inbox, err := f.db.NotificationsForUser(t.Context(), f.author.ID, false, 10)
	if err != nil {
		t.Fatalf("NotificationsForUser: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("%d notifications survived the rule, want 1", len(inbox))
	}
	if inbox[0].RuleID != 0 || inbox[0].RuleName != "" {
		t.Errorf("the notification still names rule %d (%q); ON DELETE SET NULL is what keeps the row and drops the pointer",
			inbox[0].RuleID, inbox[0].RuleName)
	}
	if inbox[0].EventType == "" {
		t.Error("the notification lost the event it was about; the join is what makes an inbox readable")
	}

	// And the rule's own history survives it, because the event is written
	// before the row goes.
	deleted, err := f.db.EventsOfType(t.Context(), events.AlertRuleDeleted, 10)
	if err != nil {
		t.Fatalf("EventsOfType: %v", err)
	}
	if len(deleted) != 1 || deleted[0].SubjectID != f.rule.ID {
		t.Errorf("a deleted rule left %d events behind", len(deleted))
	}
}

// TestDeliverMovesTheCursorWithTheRows is the property the whole file exists
// for: the notifications and the cursor that acknowledges their events are one
// transaction, so an event is evaluated exactly once.
func TestDeliverMovesTheCursorWithTheRows(t *testing.T) {
	f := newAlertFixture(t)
	f.create(t, "First")

	first, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	if len(first.Events) == 0 {
		t.Fatal("nothing is waiting")
	}
	if _, moved, err := f.db.Deliver(t.Context(), first.Cursor, first.Next(), nil, f.now); err != nil || !moved {
		t.Fatalf("Deliver = (%t, %v)", moved, err)
	}

	// A second read sees nothing: the cursor moved past them even though the
	// batch produced no notifications. An installation with no rules must not
	// accumulate a backlog against the day somebody writes one.
	second, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	if len(second.Events) != 0 {
		t.Errorf("%d events are still waiting after the cursor moved past them", len(second.Events))
	}
	if second.Cursor != first.Next() {
		t.Errorf("cursor = %d, want %d", second.Cursor, first.Next())
	}

	// A new event is picked up from where it left off.
	f.create(t, "Second")
	third, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	if len(third.Events) == 0 {
		t.Error("an event written after the cursor moved was not picked up")
	}
	for _, e := range third.Events {
		if e.ID <= second.Cursor {
			t.Errorf("event %d is at or behind the cursor %d", e.ID, second.Cursor)
		}
	}
}

// TestDeliverIsACompareAndSwap is what stops two dispatchers delivering the
// same batch. The second one is told the cursor moved, and its notifications
// roll back with it -- an inbox that repeated itself would be the visible
// half of the same bug.
func TestDeliverIsACompareAndSwap(t *testing.T) {
	f := newAlertFixture(t)
	f.create(t, "Contested")

	batch, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	note := func() []domain.NewNotification {
		return []domain.NewNotification{{
			UID: ids.MustNew(f.now), UserID: f.author.ID, EventID: batch.Events[0].ID,
			RuleID: f.rule.ID, CreatedAt: f.now,
		}}
	}

	if written, moved, err := f.db.Deliver(t.Context(), batch.Cursor, batch.Next(), note(), f.now); err != nil || !moved || written != 1 {
		t.Fatalf("the first delivery = (%d, %t, %v)", written, moved, err)
	}

	// The same batch again, as a second dispatcher that read before the first
	// one committed would have it.
	written, moved, err := f.db.Deliver(t.Context(), batch.Cursor, batch.Next(), note(), f.now)
	if err != nil {
		t.Fatalf("the second delivery failed rather than losing the swap: %v", err)
	}
	if moved || written != 0 {
		t.Errorf("the second delivery reported (%d, %t); it must lose the compare-and-swap and write nothing", written, moved)
	}

	inbox, err := f.db.NotificationsForUser(t.Context(), f.author.ID, false, 10)
	if err != nil {
		t.Fatalf("NotificationsForUser: %v", err)
	}
	if len(inbox) != 1 {
		t.Errorf("%d notifications, want 1: a batch delivered twice is an inbox that repeats itself", len(inbox))
	}
}

// TestNotificationsOnceIndex is the schema's belt to the transaction's braces:
// one notification per person per event per rule, whatever asks for a second.
func TestNotificationsOnceIndex(t *testing.T) {
	f := newAlertFixture(t)
	f.create(t, "Twice")

	batch, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	same := domain.NewNotification{
		UID: ids.MustNew(f.now), UserID: f.author.ID, EventID: batch.Events[0].ID,
		RuleID: f.rule.ID, CreatedAt: f.now,
	}
	second := same
	second.UID = ids.MustNew(f.now.Add(time.Millisecond))

	written, moved, err := f.db.Deliver(t.Context(), batch.Cursor, batch.Next(),
		[]domain.NewNotification{same, second}, f.now)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !moved {
		t.Fatal("the cursor did not move")
	}
	if written != 1 {
		t.Errorf("wrote %d rows for the same person, event, and rule, want 1", written)
	}
}

// TestNotificationReadState covers the inbox: unread filtering, the count
// beside the bell, and reading one twice.
func TestNotificationReadState(t *testing.T) {
	f := newAlertFixture(t)
	f.create(t, "Read Me")

	batch, err := f.db.PendingAlertEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("PendingAlertEvents: %v", err)
	}
	notes := make([]domain.NewNotification, 0, len(batch.Events))
	for i, e := range batch.Events {
		notes = append(notes, domain.NewNotification{
			UID:    ids.MustNew(f.now.Add(time.Duration(i) * time.Millisecond)),
			UserID: f.author.ID, EventID: e.ID, RuleID: f.rule.ID, CreatedAt: f.now,
		})
	}
	if _, moved, err := f.db.Deliver(t.Context(), batch.Cursor, batch.Next(), notes, f.now); err != nil || !moved {
		t.Fatalf("Deliver = (%t, %v)", moved, err)
	}

	unread, err := f.db.CountUnreadNotifications(t.Context(), f.author.ID)
	if err != nil {
		t.Fatalf("CountUnreadNotifications: %v", err)
	}
	if unread != len(notes) {
		t.Fatalf("unread = %d, want %d", unread, len(notes))
	}

	inbox, err := f.db.NotificationsForUser(t.Context(), f.author.ID, true, 100)
	if err != nil {
		t.Fatalf("NotificationsForUser: %v", err)
	}
	if len(inbox) != len(notes) {
		t.Fatalf("the unread listing returned %d of %d", len(inbox), len(notes))
	}
	// Newest first, which is what an inbox is.
	if len(inbox) > 1 && inbox[0].ID < inbox[1].ID {
		t.Error("the inbox is not newest first")
	}

	later := f.now.Add(time.Hour)
	read, marked, err := f.db.MarkNotificationRead(t.Context(), inbox[0].UID, f.author.ID, later)
	if err != nil || !marked {
		t.Fatalf("MarkNotificationRead = (%t, %v)", marked, err)
	}
	if !read.Read() || !read.ReadAt.Equal(later) {
		t.Errorf("read_at = %v, want %v", read.ReadAt, later)
	}

	// Reading it twice changes nothing and is not an error, which is the
	// answer ResolveComment gives to a second resolution.
	again, marked, err := f.db.MarkNotificationRead(t.Context(), inbox[0].UID, f.author.ID, later.Add(time.Hour))
	if err != nil {
		t.Fatalf("reading a notification twice failed: %v", err)
	}
	if marked {
		t.Error("the second read reported that it marked something")
	}
	if !again.ReadAt.Equal(later) {
		t.Errorf("the second read moved read_at to %v; the first read is when it was read", again.ReadAt)
	}

	// Somebody else's notification is not found rather than refused: an inbox
	// is a person's, and a route that could tell the two apart would be a
	// route that enumerates other people's mail.
	if _, _, err := f.db.MarkNotificationRead(t.Context(), inbox[0].UID, f.other.ID, later); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("reading somebody else's notification returned %v, want not found", err)
	}
}

// TestUsersWithRole is what a "role:" target resolves to, and it is the join
// an alert about "the legal desk" depends on.
func TestUsersWithRole(t *testing.T) {
	f := newAlertFixture(t)

	role, err := f.db.CreateRole(t.Context(), "legal", "Legal")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := f.db.AssignRole(t.Context(), f.author.ID, role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	users, err := f.db.UsersWithRole(t.Context(), "legal")
	if err != nil {
		t.Fatalf("UsersWithRole: %v", err)
	}
	if len(users) != 1 || users[0].ID != f.author.ID {
		t.Fatalf("UsersWithRole returned %d users, want the one who holds it", len(users))
	}
	if users[0].PasswordHash != "" {
		t.Error("UsersWithRole returned a password hash; it leaves the store on its way into the verifier and nowhere else")
	}

	// A role nobody holds notifies nobody and is not an error: an alert that
	// stopped working the day the last person left the team would break on
	// the day it matters most.
	empty, err := f.db.UsersWithRole(t.Context(), "nobody-holds-this")
	if err != nil {
		t.Fatalf("UsersWithRole on an empty role: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("UsersWithRole returned %d users for a role nobody holds", len(empty))
	}
}
