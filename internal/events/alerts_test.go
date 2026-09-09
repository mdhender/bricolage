// Copyright (c) 2026 Michael D Henderson.

package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
)

// The dispatcher's own tests: the cases that are about the loop rather than
// about the rules.
//
// They run against a fake Store, which is what the interface in alerts.go is
// for -- these are the failures a real database will not produce on demand: a
// batch another dispatcher took, a rule pointing at a user who has been
// removed, a channel this binary has no deliverer for. The rules themselves
// are exercised over real events in internal/service.

var alertStart = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// fakeStore is a Store a test drives by hand.
type fakeStore struct {
	batch domain.AlertBatch
	rules []domain.AlertRule
	users map[string]domain.User
	byID  map[int64]domain.User
	roles map[string][]domain.User

	// delivered records what Deliver was asked to write, and moved is what it
	// reports about the cursor.
	delivered [][]domain.NewNotification
	moved     bool

	// err, when set, is returned by PendingAlertEvents.
	err error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users: map[string]domain.User{},
		byID:  map[int64]domain.User{},
		roles: map[string][]domain.User{},
		moved: true,
	}
}

func (f *fakeStore) PendingAlertEvents(context.Context, int) (domain.AlertBatch, error) {
	return f.batch, f.err
}

func (f *fakeStore) ActiveAlertRules(context.Context) ([]domain.AlertRule, error) {
	return f.rules, nil
}

func (f *fakeStore) Deliver(_ context.Context, _, _ int64, notes []domain.NewNotification, _ time.Time) (int, bool, error) {
	f.delivered = append(f.delivered, notes)
	if !f.moved {
		return 0, false, nil
	}
	return len(notes), true, nil
}

func (f *fakeStore) UserByID(_ context.Context, id int64) (domain.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return domain.User{}, notFoundf("user %d", id)
}

func (f *fakeStore) UserByUID(_ context.Context, uid string) (domain.User, error) {
	if u, ok := f.users[uid]; ok {
		return u, nil
	}
	return domain.User{}, notFoundf("user %q", uid)
}

func (f *fakeStore) UsersWithRole(_ context.Context, slug string) ([]domain.User, error) {
	return f.roles[slug], nil
}

func (f *fakeStore) DocumentByID(_ context.Context, id int64) (domain.Document, error) {
	return domain.Document{}, notFoundf("document %d", id)
}

func (f *fakeStore) VersionByID(_ context.Context, id int64) (domain.Version, error) {
	return domain.Version{}, notFoundf("version %d", id)
}

// notFoundf is the store's own "no such row", which is what the dispatcher
// inspects with errors.Is before deciding a missing row is not a failure.
func notFoundf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, domain.ErrNotFound)...)
}

func (f *fakeStore) user(uid, email string, id int64) domain.User {
	u := domain.User{ID: id, UID: uid, Email: email, Name: email}
	f.users[uid] = u
	f.byID[id] = u
	return u
}

// dispatcher builds one over the fake, with a deliverer the test holds.
func newTestDispatcher(t *testing.T, db Store, deliverers map[domain.AlertChannel]Deliverer) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(DispatcherOptions{
		DB:         db,
		Clock:      clock.NewFake(alertStart),
		Deliverers: deliverers,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

// failing always errors, which is what PLAN.md M12 acceptance 4 asks for.
type failing struct{ calls int }

func (f *failing) Deliver(context.Context, Delivery) error {
	f.calls++
	return errors.New("the relay refused the message")
}

// TestAFailingDelivererIsCountedNotPropagated is the half of PLAN.md M12
// acceptance 4 that is about the loop: a channel that always errors leaves the
// pass successful and the batch acknowledged. The half about the transition it
// could not roll back is in internal/service.
func TestAFailingDelivererIsCountedNotPropagated(t *testing.T) {
	db := newFakeStore()
	db.user("u1", "legal@example.com", 1)
	db.batch = domain.AlertBatch{Cursor: 0, Events: []domain.Event{
		{ID: 1, Type: DocumentPublished, SubjectKind: domain.SubjectDocument, SubjectID: 9, OccurredAt: alertStart},
	}}
	db.rules = []domain.AlertRule{{
		ID: 1, UID: "r1", Name: "Mail somebody", EventType: DocumentPublished,
		Channel: domain.ChannelEmail, Target: "user:u1", Active: true,
	}}

	relay := &failing{}
	d := newTestDispatcher(t, db, map[domain.AlertChannel]Deliverer{domain.ChannelEmail: relay})

	res, err := d.Once(t.Context())
	if err != nil {
		t.Fatalf("a failing channel failed the pass: %v", err)
	}
	if relay.calls != 1 {
		t.Errorf("the channel was called %d times, want 1", relay.calls)
	}
	if res.Delivered != 1 || res.Failed != 1 {
		t.Errorf("delivered = %d, failed = %d", res.Delivered, res.Failed)
	}
	// The cursor moved: a channel that is down must not make the dispatcher
	// redeliver everything behind it when it comes back.
	if len(db.delivered) != 1 {
		t.Errorf("Deliver was called %d times", len(db.delivered))
	}
	// An e-mail rule writes no notification row. In-app is the channel that
	// does, and it does it inside the transaction that advances the cursor.
	if len(db.delivered[0]) != 0 {
		t.Errorf("an e-mail rule wrote %d notification rows", len(db.delivered[0]))
	}
}

// TestAChannelWithNoDelivererIsReported keeps invariant 6's spirit at run
// time: a rule naming a channel this binary cannot deliver is a rule somebody
// believes is working, so it is counted as a failure and logged rather than
// dropped.
func TestAChannelWithNoDelivererIsReported(t *testing.T) {
	db := newFakeStore()
	db.user("u1", "legal@example.com", 1)
	db.batch = domain.AlertBatch{Events: []domain.Event{
		{ID: 1, Type: DocumentPublished, SubjectKind: domain.SubjectDocument, SubjectID: 9, OccurredAt: alertStart},
	}}
	db.rules = []domain.AlertRule{{
		ID: 1, UID: "r1", Name: "Nowhere", EventType: DocumentPublished,
		Channel: domain.ChannelEmail, Target: "user:u1", Active: true,
	}}

	d := newTestDispatcher(t, db, map[domain.AlertChannel]Deliverer{})
	res, err := d.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Delivered != 1 || res.Failed != 1 {
		t.Errorf("delivered = %d, failed = %d; a channel with no deliverer is a failure, not a silence",
			res.Delivered, res.Failed)
	}
}

// TestAnotherDispatcherTookTheBatch: the compare-and-swap lost, so nothing
// leaves this process either. Whoever won it is delivering.
func TestAnotherDispatcherTookTheBatch(t *testing.T) {
	db := newFakeStore()
	db.moved = false
	db.user("u1", "legal@example.com", 1)
	db.batch = domain.AlertBatch{Events: []domain.Event{
		{ID: 1, Type: DocumentPublished, SubjectKind: domain.SubjectDocument, SubjectID: 9, OccurredAt: alertStart},
	}}
	db.rules = []domain.AlertRule{{
		ID: 1, UID: "r1", Name: "Mail somebody", EventType: DocumentPublished,
		Channel: domain.ChannelEmail, Target: "user:u1", Active: true,
	}}

	relay := &failing{}
	d := newTestDispatcher(t, db, map[domain.AlertChannel]Deliverer{domain.ChannelEmail: relay})
	res, err := d.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if relay.calls != 0 {
		t.Errorf("a batch this dispatcher lost was delivered anyway (%d calls)", relay.calls)
	}
	if res != (Result{}) {
		t.Errorf("Once reported %+v for a batch it lost", res)
	}
}

// TestATargetNamingNobodyIsNotAFailure: a rule pointing at somebody who has
// left notifies nobody, and the pass carries on. Taking the dispatcher down
// over one stale rule would stop every alert in the system.
func TestATargetNamingNobodyIsNotAFailure(t *testing.T) {
	db := newFakeStore()
	db.batch = domain.AlertBatch{Events: []domain.Event{
		{ID: 1, Type: DocumentPublished, SubjectKind: domain.SubjectDocument, SubjectID: 9, OccurredAt: alertStart},
	}}
	db.rules = []domain.AlertRule{
		{ID: 1, UID: "r1", Name: "Gone", EventType: DocumentPublished,
			Channel: domain.ChannelInApp, Target: "user:departed", Active: true},
		{ID: 2, UID: "r2", Name: "Empty desk", EventType: DocumentPublished,
			Channel: domain.ChannelInApp, Target: "role:nobody-holds-this", Active: true},
	}

	d := newTestDispatcher(t, db, nil)
	res, err := d.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Events != 1 || res.Notified != 0 {
		t.Errorf("Once reported %+v; a target naming nobody notifies nobody", res)
	}
	if len(db.delivered) != 1 || len(db.delivered[0]) != 0 {
		t.Errorf("Deliver was asked to write %v", db.delivered)
	}
}

// TestAnEmptyBatchDoesNothing: an idle dispatcher must not write, and must not
// call Deliver on a cursor it has nothing to move.
func TestAnEmptyBatchDoesNothing(t *testing.T) {
	db := newFakeStore()
	d := newTestDispatcher(t, db, nil)

	res, err := d.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("an empty pass reported %+v", res)
	}
	if len(db.delivered) != 0 {
		t.Error("an empty pass called Deliver")
	}
}

// TestRunStopsWhenItsContextEnds is the contract internal/server relies on:
// Run returns when its context ends, which is what lets the dispatcher stop on
// the one shutdown path with the job workers (invariant 17).
func TestRunStopsWhenItsContextEnds(t *testing.T) {
	db := newFakeStore()
	d, err := NewDispatcher(DispatcherOptions{
		DB:    db,
		Clock: clock.NewFake(alertStart),
		Poll:  time.Hour, // never elapses; the cancellation is what returns
		Wait:  func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); return false },
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on an ordinary shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
}

// TestNewDispatcherRefusesWhatItCannotRun keeps the two required options
// required. A dispatcher with no clock would read the wall clock somewhere,
// which is invariant 3.
func TestNewDispatcherRefusesWhatItCannotRun(t *testing.T) {
	if _, err := NewDispatcher(DispatcherOptions{Clock: clock.NewFake(alertStart)}); err == nil {
		t.Error("a dispatcher was built with no database")
	}
	if _, err := NewDispatcher(DispatcherOptions{DB: newFakeStore()}); err == nil {
		t.Error("a dispatcher was built with no clock")
	}
}

// TestLogDelivererNeverFails: the default e-mail channel writes a line and
// returns. Reporting an error would make every default installation log a
// failure for working exactly as shipped.
func TestLogDelivererNeverFails(t *testing.T) {
	d := LogDeliverer{Log: slog.New(slog.DiscardHandler)}
	if err := d.Deliver(t.Context(), Delivery{
		Rule:  domain.AlertRule{Name: "n", Channel: domain.ChannelEmail},
		Event: domain.Event{Type: DocumentPublished},
	}); err != nil {
		t.Errorf("LogDeliverer.Deliver = %v", err)
	}
	// And with no logger at all, which is what a zero value produces.
	if err := (LogDeliverer{}).Deliver(t.Context(), Delivery{}); err != nil {
		t.Errorf("a LogDeliverer with no logger = %v", err)
	}
}
