// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/service"
)

// The alert-rule and notification routes (DESIGN.md 10 and 12, PLAN.md M12).
//
// DESIGN.md 12 listed the two notification routes and no rule routes at all,
// which would have left the engine configurable only by somebody with a SQLite
// shell. earl is the acceptance-test harness for every milestone and if earl
// cannot do it the API is incomplete, so the five rule routes are an addition
// to that list and the design document has been corrected to match.
//
// The read route speaks uid rather than the {id} DESIGN.md 12 spelled, which
// is the correction migration 0008 made for jobs: invariant 10 says the API
// speaks uid only, and an invariant outranks a path in an example.

// alertRuleResponse is one rule as the API speaks it.
type alertRuleResponse struct {
	UID  string `json:"uid"`
	Name string `json:"name"`

	// EventType is the constant and EventName its display name, so a client
	// can draw a table without carrying a copy of the registry.
	EventType string `json:"event_type"`
	EventName string `json:"event_name,omitempty"`

	// Conditions are the domain values verbatim. They have JSON tags of their
	// own and one shape on the way in and on the way out, which is what keeps
	// "post what you got back" true for a rule somebody is editing.
	Conditions []domain.Condition `json:"conditions"`

	Channel string `json:"channel"`
	Target  string `json:"target"`
	Active  bool   `json:"active"`

	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedByName string    `json:"created_by_name,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func newAlertRuleResponse(r domain.AlertRule, lookup documentLookup) alertRuleResponse {
	out := alertRuleResponse{
		UID:        r.UID,
		Name:       r.Name,
		EventType:  r.EventType,
		EventName:  events.Name(r.EventType),
		Conditions: r.Conditions,
		Channel:    string(r.Channel),
		Target:     r.Target,
		Active:     r.Active,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
	if out.Conditions == nil {
		// A rule with no conditions matches every event of its type, which is
		// a real configuration. "[]" says so; "null" reads like a field the
		// server forgot to fill in.
		out.Conditions = []domain.Condition{}
	}
	if r.CreatedBy != 0 {
		out.CreatedBy, out.CreatedByName = lookup(r.CreatedBy)
	}
	return out
}

// alertRulesResponse is GET /api/v1/alert-rules.
//
// EventTypes is the vocabulary a rule may watch. It is here because the
// alternative is a client with a hard-coded copy of internal/events, which is
// the seeded 153-row registry the design replaced with a Go constant coming
// back in another form.
type alertRulesResponse struct {
	Rules      []alertRuleResponse `json:"rules"`
	EventTypes []eventTypeResponse `json:"event_types"`
}

type eventTypeResponse struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// alertRuleRequest is the body of POST and PATCH /api/v1/alert-rules.
//
// Every field is a pointer, so that PATCH can leave one alone and POST can
// tell "active: false" from "active not given". POST requires the four with no
// default.
type alertRuleRequest struct {
	Name       *string             `json:"name"`
	EventType  *string             `json:"event_type"`
	Conditions *[]domain.Condition `json:"conditions"`
	Channel    *string             `json:"channel"`
	Target     *string             `json:"target"`
	Active     *bool               `json:"active"`
}

func (req alertRuleRequest) input() service.AlertRuleInput {
	return service.AlertRuleInput{
		Name:       req.Name,
		EventType:  req.EventType,
		Conditions: req.Conditions,
		Channel:    req.Channel,
		Target:     req.Target,
		Active:     req.Active,
	}
}

// listAlertRules is GET /api/v1/alert-rules.
func (h *Handler) listAlertRules(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	rules, err := h.svc.AlertRules(r.Context(), identity)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	lookup := h.lookup(r)
	out := alertRulesResponse{
		Rules:      make([]alertRuleResponse, 0, len(rules)),
		EventTypes: make([]eventTypeResponse, 0, len(events.All())),
	}
	for _, rule := range rules {
		out.Rules = append(out.Rules, newAlertRuleResponse(rule, lookup))
	}
	for _, t := range events.All() {
		out.EventTypes = append(out.EventTypes, eventTypeResponse{Type: t, Name: events.Name(t)})
	}
	writeJSON(w, http.StatusOK, out)
}

// showAlertRule is GET /api/v1/alert-rules/{uid}.
func (h *Handler) showAlertRule(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	rule, err := h.svc.AlertRule(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newAlertRuleResponse(rule, h.lookup(r)))
}

// createAlertRule is POST /api/v1/alert-rules.
//
// A rule whose "matches" pattern does not compile is refused here, with a 422
// naming the pattern (PLAN.md M12 acceptance 3). That is the whole point of
// validating at save time: the person who typed the regexp is on the other end
// of this response, and at fire time nobody would be.
func (h *Handler) createAlertRule(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req alertRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	rule, err := h.svc.CreateAlertRule(r.Context(), identity, req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newAlertRuleResponse(rule, h.lookup(r)))
}

// patchAlertRule is PATCH /api/v1/alert-rules/{uid}.
func (h *Handler) patchAlertRule(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req alertRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	rule, err := h.svc.UpdateAlertRule(r.Context(), identity, r.PathValue("uid"), req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newAlertRuleResponse(rule, h.lookup(r)))
}

// deleteAlertRule is DELETE /api/v1/alert-rules/{uid}.
//
// There is a DELETE here where there is none on an output channel or an
// element type, and the schema is what says why: deleting one of those would
// orphan every document pointing at it, and notifications.rule_id is
// ON DELETE SET NULL -- the rows a rule produced survive it, carrying no rule.
// Deleting a rule destroys nothing but the rule.
func (h *Handler) deleteAlertRule(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := h.svc.DeleteAlertRule(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notificationResponse is one thing somebody has been told.
//
// The subject's integer id is deliberately not in it (invariant 10). What
// identifies the thing an event was about, to a client, is the uid its payload
// carries -- every event type in this system writes one -- and the subject
// kind beside it says what sort of uid that is.
type notificationResponse struct {
	UID string `json:"uid"`

	Read      bool       `json:"read"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`

	// Rule is the rule that produced it, absent once that rule is deleted.
	Rule     string `json:"rule,omitempty"`
	RuleName string `json:"rule_name,omitempty"`

	EventType   string         `json:"event_type"`
	EventName   string         `json:"event_name,omitempty"`
	SubjectKind string         `json:"subject_kind"`
	Payload     map[string]any `json:"payload,omitempty"`
	OccurredAt  time.Time      `json:"occurred_at"`
}

// notificationsResponse is GET /api/v1/notifications.
type notificationsResponse struct {
	// Unread is every unread notification this person has, not the number in
	// Notifications: a client draws it beside a bell, and a list that has been
	// limited or filtered cannot produce it.
	Unread int `json:"unread"`

	Notifications []notificationResponse `json:"notifications"`
}

func newNotificationResponse(n domain.Notification) notificationResponse {
	out := notificationResponse{
		UID:         n.UID,
		Read:        n.Read(),
		CreatedAt:   n.CreatedAt,
		Rule:        n.RuleUID,
		RuleName:    n.RuleName,
		EventType:   n.EventType,
		EventName:   events.Name(n.EventType),
		SubjectKind: n.SubjectKind,
		Payload:     n.Payload,
		OccurredAt:  n.OccurredAt,
	}
	if n.Read() {
		at := n.ReadAt
		out.ReadAt = &at
	}
	return out
}

// listNotifications is GET /api/v1/notifications?unread&limit=N.
func (h *Handler) listNotifications(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	unread, err := boolParam(r, "unread")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	limit, err := intParam(r, "limit", 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	inbox, err := h.svc.Notifications(r.Context(), identity, unread, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := notificationsResponse{
		Unread:        inbox.Unread,
		Notifications: make([]notificationResponse, 0, len(inbox.Notifications)),
	}
	for _, n := range inbox.Notifications {
		out.Notifications = append(out.Notifications, newNotificationResponse(n))
	}
	writeJSON(w, http.StatusOK, out)
}

// readNotification is POST /api/v1/notifications/{uid}/read.
//
// It answers 200 both times. Reading a notification twice created nothing and
// changed nothing the second time, and a 409 would be telling somebody their
// inbox was in conflict with itself.
func (h *Handler) readNotification(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	n, _, err := h.svc.MarkNotificationRead(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newNotificationResponse(n))
}
