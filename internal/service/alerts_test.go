// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// Alerts end to end, at the level where they can be asserted: a real store,
// real events written by the ordinary operations, and the dispatcher reading
// them back (PLAN.md M12).
//
// The dispatcher belongs to internal/events, but its behaviour is only
// interesting over events something wrote, and this is the package that has a
// harness which writes them. The tests that need no service -- a channel that
// always errors, a batch another dispatcher took -- live beside it in
// internal/events, against a Store this package's store satisfies.

// alertHarness is a service with an element type, an editor who may do
// everything, a member of the legal desk, and a bystander who should never
// hear about any of it.
type alertHarness struct {
	*harness
	editor    domain.Identity
	legal     domain.Identity
	bystander domain.Identity

	// dispatcher is the thing under test. Its deliverers are replaced per
	// test when the test is about a channel.
	dispatcher *events.Dispatcher
	delivered  *recordingDeliverer
}

// recordingDeliverer stands in for the e-mail channel. It records what it was
// asked to send and, when told to, fails every time.
type recordingDeliverer struct {
	fail bool
	sent []events.Delivery
}

func (d *recordingDeliverer) Deliver(_ context.Context, n events.Delivery) error {
	d.sent = append(d.sent, n)
	if d.fail {
		return errors.New("the mail relay is down")
	}
	return nil
}

func newAlertHarness(t *testing.T) *alertHarness {
	t.Helper()
	h := newHarness(t)
	h.elementType(t, "story", domain.KindStory)

	a := &alertHarness{
		harness:   h,
		editor:    h.admin(t, "editor@example.com", "correct horse battery", domain.Publish),
		legal:     h.admin(t, "legal@example.com", "correct horse battery", domain.Read),
		bystander: h.admin(t, "bystander@example.com", "correct horse battery", domain.Read),
		delivered: &recordingDeliverer{},
	}
	a.role(t, "legal", a.legal)

	dispatcher, err := events.NewDispatcher(events.DispatcherOptions{
		DB:    h.db,
		Clock: h.clock,
		Deliverers: map[domain.AlertChannel]events.Deliverer{
			domain.ChannelEmail: a.delivered,
		},
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	a.dispatcher = dispatcher

	// Everything written while the harness was being built is water under the
	// bridge: a rule takes effect from when it is written, not retroactively.
	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("draining the events written by the fixture: %v", err)
	}
	return a
}

// role gives an identity a named role, which is what a "role:" target
// resolves through.
func (a *alertHarness) role(t *testing.T, slug string, who domain.Identity) {
	t.Helper()
	r, err := a.db.CreateRole(t.Context(), slug, slug)
	if err != nil {
		t.Fatalf("CreateRole(%q): %v", slug, err)
	}
	if err := a.db.AssignRole(t.Context(), who.User.ID, r.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
}

// rule writes an alert rule as the editor, who holds Create over everything.
func (a *alertHarness) rule(t *testing.T, name, eventType, channel, target string, conds ...domain.Condition) domain.AlertRule {
	t.Helper()
	r, err := a.CreateAlertRule(t.Context(), a.editor, AlertRuleInput{
		Name: &name, EventType: &eventType, Channel: &channel, Target: &target,
		Conditions: &conds,
	})
	if err != nil {
		t.Fatalf("CreateAlertRule(%q): %v", name, err)
	}
	return r
}

// inReview creates a document, checks it in, and moves it to review, which is
// the transition every rule below is about. The default workflow's states are
// draft, review, approved, published, archived; "review" is where PLAN.md
// M12's criterion writes "legal".
func (a *alertHarness) inReview(t *testing.T, title, slug string) domain.Document {
	t.Helper()
	view, err := a.CreateDocument(t.Context(), a.editor, domain.NewDocument{
		SiteID: a.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
		Title: title, Slug: slug, CoverDate: "2026-03-01",
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	uid := view.Document.UID
	if _, err := a.Checkout(t.Context(), a.editor, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := a.Checkin(t.Context(), a.editor, uid, "first cut"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	moved, err := a.Transition(t.Context(), a.editor, uid, "review", "")
	if err != nil {
		t.Fatalf("Transition to review: %v", err)
	}
	return moved.Document
}

// inbox is somebody's notifications, newest first.
func (a *alertHarness) inbox(t *testing.T, who domain.Identity) Inbox {
	t.Helper()
	box, err := a.Notifications(t.Context(), who, false, 100)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	return box
}

// TestARuleNotifiesItsRecipientsAndNobodyElse is PLAN.md M12 acceptance 1.
//
// The criterion is written with a "legal" state; the default workflow this
// repository seeds calls that state "review", and the payload key a transition
// writes is "to" rather than "to_state" (internal/workflow/engine.go). The
// property under test is the criterion's exactly: a rule on
// document.transitioned, conditioned on the destination state, notifies the
// people its target names and nobody else.
func TestARuleNotifiesItsRecipientsAndNobodyElse(t *testing.T) {
	a := newAlertHarness(t)
	a.rule(t, "Into review", events.DocumentTransitioned, string(domain.ChannelInApp), "role:legal",
		domain.Condition{Field: "to", Op: domain.OpEq, Value: "review"})

	doc := a.inReview(t, "Budget Explained", "budget-explained")

	res, err := a.dispatcher.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Notified != 1 {
		t.Fatalf("wrote %d notifications, want 1 (the legal desk has one member)", res.Notified)
	}

	box := a.inbox(t, a.legal)
	if len(box.Notifications) != 1 {
		t.Fatalf("the legal desk has %d notifications, want 1", len(box.Notifications))
	}
	got := box.Notifications[0]
	if got.EventType != events.DocumentTransitioned {
		t.Errorf("notified about %q, want %q", got.EventType, events.DocumentTransitioned)
	}
	if got.Payload["uid"] != doc.UID {
		t.Errorf("the notification is about %v, want document %s", got.Payload["uid"], doc.UID)
	}
	if got.RuleName != "Into review" {
		t.Errorf("rule name = %q", got.RuleName)
	}
	if box.Unread != 1 {
		t.Errorf("unread = %d, want 1", box.Unread)
	}

	// And nobody else. The editor caused the transition and the bystander was
	// never named; neither is on the legal desk.
	for _, who := range []struct {
		name string
		id   domain.Identity
	}{{"the editor who moved it", a.editor}, {"a bystander", a.bystander}} {
		if n := len(a.inbox(t, who.id).Notifications); n != 0 {
			t.Errorf("%s got %d notifications, want none", who.name, n)
		}
	}
}

// TestAllConditionsMustPass is PLAN.md M12 acceptance 2: a rule with two
// conditions where one fails fires nothing.
func TestAllConditionsMustPass(t *testing.T) {
	a := newAlertHarness(t)

	// Both true: the destination is review and the document is a story.
	a.rule(t, "Both", events.DocumentTransitioned, string(domain.ChannelInApp), "user:"+a.legal.User.UID,
		domain.Condition{Field: "to", Op: domain.OpEq, Value: "review"},
		domain.Condition{Field: "kind", Op: domain.OpEq, Value: "story"})

	// One false: the same destination, the wrong kind.
	a.rule(t, "One of two", events.DocumentTransitioned, string(domain.ChannelInApp), "user:"+a.bystander.User.UID,
		domain.Condition{Field: "to", Op: domain.OpEq, Value: "review"},
		domain.Condition{Field: "kind", Op: domain.OpEq, Value: "media"})

	a.inReview(t, "Two Conditions", "two-conditions")
	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	if n := len(a.inbox(t, a.legal).Notifications); n != 1 {
		t.Errorf("the rule whose conditions all pass produced %d notifications, want 1", n)
	}
	if n := len(a.inbox(t, a.bystander).Notifications); n != 0 {
		t.Errorf("the rule with one failing condition produced %d notifications; conditions are an AND", n)
	}
}

// TestAnInvalidPatternIsRefusedWhenTheRuleIsSaved is PLAN.md M12 acceptance 3
// at the use-case level: the refusal is ErrInvalid -- a 422 at the edge -- and
// its message names the pattern, because the person reading it typed it.
func TestAnInvalidPatternIsRefusedWhenTheRuleIsSaved(t *testing.T) {
	a := newAlertHarness(t)

	name, eventType := "Broken", events.DocumentPublished
	channel, target := string(domain.ChannelInApp), "role:legal"
	conds := []domain.Condition{{Field: "title", Op: domain.OpMatches, Value: "([unclosed"}}

	_, err := a.CreateAlertRule(t.Context(), a.editor, AlertRuleInput{
		Name: &name, EventType: &eventType, Channel: &channel, Target: &target, Conditions: &conds,
	})
	if err == nil {
		t.Fatal("a rule with an uncompilable pattern was saved")
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error is %v; a bad pattern is malformed input", err)
	}
	if !strings.Contains(err.Error(), "([unclosed") {
		t.Errorf("error %q does not name the pattern", err)
	}

	// Nothing was written, so nothing can fire later.
	rules, err := a.AlertRules(t.Context(), a.editor)
	if err != nil {
		t.Fatalf("AlertRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("%d rules exist after a refusal", len(rules))
	}

	// An event type this system never writes is refused for the same reason
	// and at the same moment: a rule watching for something that cannot
	// happen is a rule somebody believes is working.
	bogus := "document.exploded"
	if _, err := a.CreateAlertRule(t.Context(), a.editor, AlertRuleInput{
		Name: &name, EventType: &bogus, Channel: &channel, Target: &target,
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an unknown event type was accepted, or refused with %v", err)
	}
}

// TestAFailingChannelDoesNotRollBackTheTransition is PLAN.md M12
// acceptance 4, with a channel that always errors.
//
// It cannot roll anything back, and the test says why rather than only that:
// the transition committed before the dispatcher read the event that records
// it, so by the time a channel fails there is no transaction left for it to be
// part of. That is DESIGN.md 6.4's rule made structural.
func TestAFailingChannelDoesNotRollBackTheTransition(t *testing.T) {
	a := newAlertHarness(t)
	a.delivered.fail = true
	a.rule(t, "Mail the desk", events.DocumentTransitioned, string(domain.ChannelEmail), "email:desk@example.com",
		domain.Condition{Field: "to", Op: domain.OpEq, Value: "review"})

	doc := a.inReview(t, "Still Moved", "still-moved")

	res, err := a.dispatcher.Once(t.Context())
	if err != nil {
		t.Fatalf("a failing channel failed the whole pass: %v", err)
	}
	if res.Delivered != 1 || res.Failed != 1 {
		t.Errorf("delivered = %d, failed = %d; want one attempt and one failure", res.Delivered, res.Failed)
	}
	if len(a.delivered.sent) != 1 || a.delivered.sent[0].Address != "desk@example.com" {
		t.Errorf("the channel was asked to send %+v", a.delivered.sent)
	}

	// The transition stands.
	after, err := a.Document(t.Context(), a.editor, doc.UID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if after.Document.State != "review" {
		t.Errorf("state = %q after a failed notification, want review", after.Document.State)
	}

	// And the batch is acknowledged rather than retried forever: a channel
	// that is down must not make the dispatcher redeliver every event behind
	// it once it comes back.
	second, err := a.dispatcher.Once(t.Context())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if second.Delivered != 0 {
		t.Errorf("the failed delivery was attempted again on the next pass (%d)", second.Delivered)
	}
}

// TestFieldResolutionOrderOverRealEvents is PLAN.md M12 acceptance 5 against
// events something actually wrote.
//
// document.created's payload carries the state the document started in, and
// the document's own row carries the state it is in now. Moving it to review
// before the dispatcher reads the event makes the two disagree, which is the
// only way to tell which one a rule resolved.
func TestFieldResolutionOrderOverRealEvents(t *testing.T) {
	a := newAlertHarness(t)

	// Resolves from the payload: "draft" is what the document.created event
	// recorded.
	a.rule(t, "As created", events.DocumentCreated, string(domain.ChannelInApp), "user:"+a.legal.User.UID,
		domain.Condition{Field: "state", Op: domain.OpEq, Value: "draft"})

	// Would resolve from the subject, which says "review" by the time the
	// dispatcher runs. It must not match: the payload wins.
	a.rule(t, "As it is now", events.DocumentCreated, string(domain.ChannelInApp), "user:"+a.bystander.User.UID,
		domain.Condition{Field: "state", Op: domain.OpEq, Value: "review"})

	a.inReview(t, "Moved Before Delivery", "moved-before-delivery")

	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if n := len(a.inbox(t, a.legal).Notifications); n != 1 {
		t.Errorf("the rule reading the payload produced %d notifications, want 1", n)
	}
	if n := len(a.inbox(t, a.bystander).Notifications); n != 0 {
		t.Errorf("the rule reading the subject produced %d notifications; the payload resolves first", n)
	}

	// The subject is still the fallback for what the payload did not write
	// down. document.created's payload has no slug; the document row does.
	a.rule(t, "By slug", events.DocumentCreated, string(domain.ChannelInApp), "user:"+a.bystander.User.UID,
		domain.Condition{Field: "slug", Op: domain.OpEq, Value: "second-story"})
	a.inReview(t, "Second Story", "second-story")
	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if n := len(a.inbox(t, a.bystander).Notifications); n != 1 {
		t.Errorf("a rule on a field only the subject carries produced %d notifications, want 1", n)
	}
}

// TestAlertRuleLifecycleWritesItsEvents is invariant 7 for M12's three
// operations: an operation that does not write its event is not finished.
func TestAlertRuleLifecycleWritesItsEvents(t *testing.T) {
	a := newAlertHarness(t)
	r := a.rule(t, "Watch publishes", events.DocumentPublished, string(domain.ChannelInApp), "role:legal")

	off := false
	updated, err := a.UpdateAlertRule(t.Context(), a.editor, r.UID, AlertRuleInput{Active: &off})
	if err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	if updated.Active {
		t.Error("the rule is still active after being switched off")
	}
	// An update leaves everything it was not given alone.
	if updated.Name != r.Name || updated.EventType != r.EventType || updated.Target != r.Target {
		t.Errorf("switching a rule off changed something else: %+v", updated)
	}

	if err := a.DeleteAlertRule(t.Context(), a.editor, r.UID); err != nil {
		t.Fatalf("DeleteAlertRule: %v", err)
	}
	if _, err := a.AlertRule(t.Context(), a.editor, r.UID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the deleted rule reads back as %v", err)
	}

	for _, want := range []string{events.AlertRuleCreated, events.AlertRuleUpdated, events.AlertRuleDeleted} {
		written, err := a.db.EventsOfType(t.Context(), want, 10)
		if err != nil {
			t.Fatalf("EventsOfType(%s): %v", want, err)
		}
		if len(written) != 1 {
			t.Errorf("%d %s events, want 1", len(written), want)
			continue
		}
		if written[0].Payload["uid"] != r.UID || written[0].ActorID != a.editor.User.ID {
			t.Errorf("%s payload = %v", want, written[0].Payload)
		}
	}
}

// TestAlertRulesAreSystemConfiguration is the authorization story: a rule
// names an audience and says what it is watched for, so writing or reading one
// needs Create over the system subject -- the same rule element types follow.
func TestAlertRulesAreSystemConfiguration(t *testing.T) {
	a := newAlertHarness(t)
	name, eventType := "Not yours", events.DocumentPublished
	channel, target := string(domain.ChannelInApp), "role:legal"

	// The legal desk member holds Read over everything and may not write one.
	if _, err := a.CreateAlertRule(t.Context(), a.legal, AlertRuleInput{
		Name: &name, EventType: &eventType, Channel: &channel, Target: &target,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a reader wrote an alert rule, or was refused with %v", err)
	}
	if _, err := a.AlertRules(t.Context(), a.legal); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a reader listed the alert rules, or was refused with %v", err)
	}

	// An inbox needs no privilege at all, because it is the caller's own and
	// there is no route to anybody else's.
	if _, err := a.Notifications(t.Context(), a.legal, false, 10); err != nil {
		t.Errorf("reading one's own inbox needed a privilege: %v", err)
	}
}

// TestReadingANotification covers the read state through the service, and the
// forgiveness a second read gets.
func TestReadingANotification(t *testing.T) {
	a := newAlertHarness(t)
	a.rule(t, "Everything published", events.DocumentTransitioned, string(domain.ChannelInApp),
		"user:"+a.legal.User.UID)
	a.inReview(t, "Read Me", "read-me")
	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	box := a.inbox(t, a.legal)
	if len(box.Notifications) != 1 || box.Unread != 1 {
		t.Fatalf("inbox = %d notifications, %d unread", len(box.Notifications), box.Unread)
	}
	uid := box.Notifications[0].UID

	n, marked, err := a.MarkNotificationRead(t.Context(), a.legal, uid)
	if err != nil || !marked || !n.Read() {
		t.Fatalf("MarkNotificationRead = (%t, %t, %v)", marked, n.Read(), err)
	}
	if _, marked, err := a.MarkNotificationRead(t.Context(), a.legal, uid); err != nil || marked {
		t.Errorf("reading it twice = (%t, %v); the second time changes nothing and is not an error", marked, err)
	}
	if box := a.inbox(t, a.legal); box.Unread != 0 {
		t.Errorf("unread = %d after reading the only one", box.Unread)
	}

	// Somebody else's is not found rather than refused.
	if _, _, err := a.MarkNotificationRead(t.Context(), a.bystander, uid); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("reading somebody else's notification returned %v, want not found", err)
	}
}
