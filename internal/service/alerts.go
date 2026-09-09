// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
)

// The alert-rule and notification use cases (PLAN.md M12).
//
// Two audiences, two rules, and the split is the whole of the authorization
// story here.
//
// An alert rule is operator configuration. It names an event type, a set of
// conditions, and an audience -- which means reading one tells you who is
// watching what, and writing one lets you put a line in somebody else's inbox
// every time a document moves. So it is resolved against the *system subject*,
// the empty domain.Subject that only a grant constraining nothing matches,
// which is the rule DESIGN.md 12 states for element types and for the job
// queue and for the same reason: a site-scoped grant says what its holder may
// do to that site's documents and says nothing about a rule every site shares.
//
// A notification is nobody's business but its owner's. There is no privilege
// for reading one, because there is no route that reads anybody else's: the
// store matches on the user as well as the uid, so somebody else's
// notification is not found rather than refused, and an inbox is not a
// resource with permissions on it.

// AlertAdmin is what writing or reading an alert rule needs, over the system
// subject. It is domain.Create for the reason ElementTypeAdmin is: this is
// configuration that applies everywhere, and the scale's Create is the level
// at which somebody is trusted to add things the whole installation uses.
const AlertAdmin = domain.Create

// AlertRuleInput is a rule somebody is asking to write.
//
// Every field is a pointer because this is the body of a PATCH as well as of a
// POST: absent means "leave it alone", and there is no other way to say that
// about a boolean whose zero value is a meaningful setting. Create requires
// the four that have no sensible default.
type AlertRuleInput struct {
	Name       *string
	EventType  *string
	Conditions *[]domain.Condition
	Channel    *string
	Target     *string
	Active     *bool
}

// AlertRules returns every rule, active and inactive alike.
func (s *Service) AlertRules(ctx context.Context, actor domain.Identity) ([]domain.AlertRule, error) {
	if err := s.mayAdministerAlerts(actor, "alert rules"); err != nil {
		return nil, err
	}
	return s.db.ListAlertRules(ctx)
}

// AlertRule reads one rule.
func (s *Service) AlertRule(ctx context.Context, actor domain.Identity, uid string) (domain.AlertRule, error) {
	if err := s.mayAdministerAlerts(actor, fmt.Sprintf("alert rule %q", uid)); err != nil {
		return domain.AlertRule{}, err
	}
	return s.db.AlertRuleByUID(ctx, uid)
}

// CreateAlertRule writes a rule.
//
// Everything that could make the rule unrunnable is checked here, before the
// row exists: the event type is one this binary writes, the channel is one it
// delivers, the target is one it can resolve, and every "matches" pattern
// compiles (PLAN.md M12 acceptance 3). A rule that is refused at fire time is
// a rule whose failure is a log line about an event that has already gone
// past, read by nobody, which is the same defect as a guard name with no
// enforcement behind it (invariant 6).
func (s *Service) CreateAlertRule(ctx context.Context, actor domain.Identity, in AlertRuleInput) (domain.AlertRule, error) {
	if err := s.mayAdministerAlerts(actor, "alert rules"); err != nil {
		return domain.AlertRule{}, err
	}
	if in.Name == nil || in.EventType == nil || in.Channel == nil || in.Target == nil {
		return domain.AlertRule{}, fmt.Errorf(
			"alert rule: a name, an event type, a channel, and a target are required: %w", domain.ErrInvalid)
	}

	now := s.Now()
	r := domain.AlertRule{
		Name:      strings.TrimSpace(*in.Name),
		EventType: strings.TrimSpace(*in.EventType),
		Channel:   domain.AlertChannel(strings.TrimSpace(*in.Channel)),
		Target:    strings.TrimSpace(*in.Target),
		Active:    true,
		CreatedBy: actor.User.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if in.Conditions != nil {
		r.Conditions = *in.Conditions
	}
	if in.Active != nil {
		r.Active = *in.Active
	}
	if err := s.validateAlertRule(r); err != nil {
		return domain.AlertRule{}, err
	}

	uid, err := ids.New(now)
	if err != nil {
		return domain.AlertRule{}, err
	}
	r.UID = uid
	return s.db.CreateAlertRule(ctx, r, domain.Event{
		Type:       events.AlertRuleCreated,
		ActorID:    actor.User.ID,
		Payload:    alertRulePayload(r),
		OccurredAt: now,
	})
}

// UpdateAlertRule changes a rule, including turning one off.
//
// Turning a rule off is an update rather than a deletion, and it is the reason
// "active" is a column: an alert switched off for the duration of a bulk
// import is a rule somebody wants back on Friday, and deleting it means
// retyping the conditions.
func (s *Service) UpdateAlertRule(ctx context.Context, actor domain.Identity, uid string, in AlertRuleInput) (domain.AlertRule, error) {
	if err := s.mayAdministerAlerts(actor, fmt.Sprintf("alert rule %q", uid)); err != nil {
		return domain.AlertRule{}, err
	}
	r, err := s.db.AlertRuleByUID(ctx, uid)
	if err != nil {
		return domain.AlertRule{}, err
	}

	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.EventType != nil {
		r.EventType = strings.TrimSpace(*in.EventType)
	}
	if in.Conditions != nil {
		r.Conditions = *in.Conditions
	}
	if in.Channel != nil {
		r.Channel = domain.AlertChannel(strings.TrimSpace(*in.Channel))
	}
	if in.Target != nil {
		r.Target = strings.TrimSpace(*in.Target)
	}
	if in.Active != nil {
		r.Active = *in.Active
	}
	if err := s.validateAlertRule(r); err != nil {
		return domain.AlertRule{}, err
	}

	now := s.Now()
	r.UpdatedAt = now
	return s.db.UpdateAlertRule(ctx, r, domain.Event{
		Type:       events.AlertRuleUpdated,
		ActorID:    actor.User.ID,
		Payload:    alertRulePayload(r),
		OccurredAt: now,
	})
}

// DeleteAlertRule removes a rule. The notifications it produced survive it,
// because notifications.rule_id is ON DELETE SET NULL: what a rule already
// told somebody still happened.
func (s *Service) DeleteAlertRule(ctx context.Context, actor domain.Identity, uid string) error {
	if err := s.mayAdministerAlerts(actor, fmt.Sprintf("alert rule %q", uid)); err != nil {
		return err
	}
	r, err := s.db.AlertRuleByUID(ctx, uid)
	if err != nil {
		return err
	}
	now := s.Now()
	return s.db.DeleteAlertRule(ctx, r.ID, domain.Event{
		Type:       events.AlertRuleDeleted,
		ActorID:    actor.User.ID,
		Payload:    alertRulePayload(r),
		OccurredAt: now,
	})
}

// validateAlertRule is everything a rule has to satisfy before it is written.
//
// The event type is checked here rather than in domain because the registry
// lives in internal/events and domain imports nothing (invariant 1). The
// refusal names the type rather than listing thirty-nine of them: the list is
// what "earl alert events" prints, and an error message is not a catalogue.
func (s *Service) validateAlertRule(r domain.AlertRule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !events.Registered(r.EventType) {
		return fmt.Errorf(
			"alert rule %q: %q is not an event type this system writes: %w",
			r.Name, r.EventType, domain.ErrInvalid)
	}
	return nil
}

func (s *Service) mayAdministerAlerts(actor domain.Identity, what string) error {
	if authz.Allows(actor.Grants, systemSubject(), AlertAdmin) {
		return nil
	}
	return fmt.Errorf("%s: %s over the system is required: %w", what, AlertAdmin, domain.ErrForbidden)
}

// alertRulePayload is what an alert rule's events carry.
//
// It names the uid, the event type watched, the channel, and the target, but
// not the conditions: the conditions are the rule's body, they can be long,
// and the question an audit asks of a notification is "who was being told
// about what", which these four answer.
func alertRulePayload(r domain.AlertRule) map[string]any {
	return map[string]any{
		"uid":        r.UID,
		"name":       r.Name,
		"event_type": r.EventType,
		"channel":    string(r.Channel),
		"target":     r.Target,
		"active":     r.Active,
		"conditions": len(r.Conditions),
	}
}

// Inbox is somebody's notifications and how many of them are unread.
//
// The count is separate from the list because a client draws "3" beside a bell
// and a list it has limited or filtered cannot produce that number -- the same
// reason the approvals response carries Count and Required rather than letting
// a client work them out.
type Inbox struct {
	Notifications []domain.Notification
	Unread        int
}

// Notifications returns the caller's own inbox, newest first.
//
// There is no way to read anybody else's, and deliberately no privilege that
// would let one: an inbox is a person's, and a route that could read another
// person's would be a route that reports what the alert rules are watching
// them do.
func (s *Service) Notifications(ctx context.Context, actor domain.Identity, unreadOnly bool, limit int) (Inbox, error) {
	list, err := s.db.NotificationsForUser(ctx, actor.User.ID, unreadOnly, limit)
	if err != nil {
		return Inbox{}, err
	}
	unread, err := s.db.CountUnreadNotifications(ctx, actor.User.ID)
	if err != nil {
		return Inbox{}, err
	}
	return Inbox{Notifications: list, Unread: unread}, nil
}

// MarkNotificationRead marks one of the caller's own notifications read.
//
// Reading one twice is not an error and not two reads: the second call changes
// nothing and returns the notification as it stands, which is the answer
// ResolveComment and Approve both give and for the same reason -- nothing
// changed, so there is nothing to report as a conflict.
func (s *Service) MarkNotificationRead(ctx context.Context, actor domain.Identity, uid string) (domain.Notification, bool, error) {
	n, marked, err := s.db.MarkNotificationRead(ctx, uid, actor.User.ID, s.Now())
	if err != nil {
		return domain.Notification{}, false, err
	}
	if !marked {
		s.log.Debug("notification already read", "notification", uid, "actor", actor.User.UID)
	}
	return n, marked, nil
}
