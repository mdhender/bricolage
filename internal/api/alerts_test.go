// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// The transport half of M12 (PLAN.md M12, DESIGN.md 12). The API is what is
// under test, so everything below it is real: the handlers go through the
// service, the service through the store, and the dispatcher over the events
// that produced.

// alertAPI is the harness plus the two people every test here needs: an
// administrator who may write rules, and somebody who may not.
type alertAPI struct {
	*harness
	admin  string // token
	reader string // token

	adminUser  domain.User
	readerUser domain.User
	dispatcher *events.Dispatcher
}

func newAlertAPI(t *testing.T) *alertAPI {
	t.Helper()
	h := newHarness(t)
	a := &alertAPI{harness: h}
	a.adminUser = h.user(t, "admin@example.com", domain.Create)
	a.readerUser = h.user(t, "reader@example.com", domain.Read)
	a.admin = h.login(t, "admin@example.com")
	a.reader = h.login(t, "reader@example.com")

	d, err := events.NewDispatcher(events.DispatcherOptions{DB: h.db, Clock: h.clock})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	a.dispatcher = d
	// Drain what building the fixture wrote: a rule takes effect from when it
	// is written.
	if _, err := d.Once(t.Context()); err != nil {
		t.Fatalf("draining: %v", err)
	}
	return a
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return out
}

// TestAlertRuleRoutes walks the five of them, in the order a person uses them.
func TestAlertRuleRoutes(t *testing.T) {
	a := newAlertAPI(t)

	var created alertRuleResponse
	t.Run("create", func(t *testing.T) {
		rec := a.do(t, http.MethodPost, "/api/v1/alert-rules", a.admin, map[string]any{
			"name":       "Into review",
			"event_type": events.DocumentTransitioned,
			"channel":    "in_app",
			"target":     "user:" + a.readerUser.UID,
			"conditions": []map[string]any{{"field": "to", "op": "eq", "value": "review"}},
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST: %d %s", rec.Code, rec.Body)
		}
		created = decode[alertRuleResponse](t, rec.Body.Bytes())
		if created.UID == "" || !created.Active || len(created.Conditions) != 1 {
			t.Fatalf("created = %+v", created)
		}
		// The display name comes off the registry so a client need not carry
		// a copy of it.
		if created.EventName != events.Name(events.DocumentTransitioned) {
			t.Errorf("event_name = %q", created.EventName)
		}
		if created.CreatedBy != a.adminUser.UID {
			t.Errorf("created_by = %q, want the uid %q (invariant 10)", created.CreatedBy, a.adminUser.UID)
		}
	})

	t.Run("list", func(t *testing.T) {
		rec := a.do(t, http.MethodGet, "/api/v1/alert-rules", a.admin, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		out := decode[alertRulesResponse](t, rec.Body.Bytes())
		if len(out.Rules) != 1 {
			t.Fatalf("%d rules, want 1", len(out.Rules))
		}
		if len(out.EventTypes) != len(events.All()) {
			t.Errorf("the listing offers %d event types, want %d", len(out.EventTypes), len(events.All()))
		}
	})

	t.Run("show", func(t *testing.T) {
		rec := a.do(t, http.MethodGet, "/api/v1/alert-rules/"+created.UID, a.admin, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		if got := decode[alertRuleResponse](t, rec.Body.Bytes()); got.UID != created.UID {
			t.Errorf("uid = %q, want %q", got.UID, created.UID)
		}
	})

	t.Run("patch leaves what it was not given alone", func(t *testing.T) {
		rec := a.do(t, http.MethodPatch, "/api/v1/alert-rules/"+created.UID, a.admin,
			map[string]any{"active": false})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rec.Code, rec.Body)
		}
		got := decode[alertRuleResponse](t, rec.Body.Bytes())
		if got.Active {
			t.Error("the rule is still active")
		}
		if got.Name != created.Name || got.Target != created.Target || len(got.Conditions) != 1 {
			t.Errorf("switching it off changed something else: %+v", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		rec := a.do(t, http.MethodDelete, "/api/v1/alert-rules/"+created.UID, a.admin, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
		}
		rec = a.do(t, http.MethodGet, "/api/v1/alert-rules/"+created.UID, a.admin, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("the deleted rule reads back as %d", rec.Code)
		}
	})
}

// TestABrokenPatternIs422 is PLAN.md M12 acceptance 3 at the edge: the refusal
// is a 422 whose detail names the pattern, which is the only place the person
// who typed it will ever see it.
func TestABrokenPatternIs422(t *testing.T) {
	a := newAlertAPI(t)
	rec := a.do(t, http.MethodPost, "/api/v1/alert-rules", a.admin, map[string]any{
		"name":       "Broken",
		"event_type": events.DocumentPublished,
		"channel":    "in_app",
		"target":     "role:legal",
		"conditions": []map[string]any{{"field": "title", "op": "matches", "value": "([unclosed"}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST: %d %s, want 422", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "([unclosed") {
		t.Errorf("the problem document does not name the pattern: %s", rec.Body)
	}
}

// TestAlertRulesNeedTheSystemPrivilege: a rule names an audience and says what
// it is watched for, so it is configuration the whole installation shares.
func TestAlertRulesNeedTheSystemPrivilege(t *testing.T) {
	a := newAlertAPI(t)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/alert-rules", nil},
		{http.MethodPost, "/api/v1/alert-rules", map[string]any{
			"name": "n", "event_type": events.DocumentPublished, "channel": "in_app", "target": "role:legal",
		}},
	} {
		rec := a.do(t, tc.method, tc.path, a.reader, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a reader: %d %s, want 403", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

// TestNotificationRoutes drives a real notification through the API: a rule, an
// event, the dispatcher, the inbox, and the read.
func TestNotificationRoutes(t *testing.T) {
	a := newAlertAPI(t)

	rec := a.do(t, http.MethodPost, "/api/v1/alert-rules", a.admin, map[string]any{
		"name":       "Every login",
		"event_type": events.SessionCreated,
		"channel":    "in_app",
		"target":     "user:" + a.readerUser.UID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST rule: %d %s", rec.Code, rec.Body)
	}

	// An event the rule watches, written by the ordinary path.
	a.login(t, "admin@example.com")
	if _, err := a.dispatcher.Once(t.Context()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	rec = a.do(t, http.MethodGet, "/api/v1/notifications", a.reader, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET notifications: %d %s", rec.Code, rec.Body)
	}
	box := decode[notificationsResponse](t, rec.Body.Bytes())
	if len(box.Notifications) != 1 || box.Unread != 1 {
		t.Fatalf("inbox = %d notifications, %d unread: %s", len(box.Notifications), box.Unread, rec.Body)
	}
	n := box.Notifications[0]
	if n.EventType != events.SessionCreated || n.Read {
		t.Errorf("notification = %+v", n)
	}

	// Invariant 10: the response speaks uids and never the internal key of
	// the thing the event was about.
	if strings.Contains(rec.Body.String(), "subject_id") {
		t.Errorf("the response carries a subject_id: %s", rec.Body)
	}

	// The inbox is the caller's own. The administrator caused the event and
	// has none.
	rec = a.do(t, http.MethodGet, "/api/v1/notifications", a.admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET notifications: %d %s", rec.Code, rec.Body)
	}
	if other := decode[notificationsResponse](t, rec.Body.Bytes()); len(other.Notifications) != 0 {
		t.Errorf("somebody the rule did not name has %d notifications", len(other.Notifications))
	}

	// Reading it, twice: 200 both times.
	for range 2 {
		rec = a.do(t, http.MethodPost, "/api/v1/notifications/"+n.UID+"/read", a.reader, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST read: %d %s", rec.Code, rec.Body)
		}
		if got := decode[notificationResponse](t, rec.Body.Bytes()); !got.Read {
			t.Error("the notification is not read after being read")
		}
	}

	// Somebody else's is not found rather than refused: a route that could
	// tell the two apart would enumerate other people's mail.
	rec = a.do(t, http.MethodPost, "/api/v1/notifications/"+n.UID+"/read", a.admin, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("reading somebody else's notification: %d, want 404", rec.Code)
	}

	// ?unread now has nothing in it, and the count agrees.
	rec = a.do(t, http.MethodGet, "/api/v1/notifications?unread", a.reader, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?unread: %d %s", rec.Code, rec.Body)
	}
	box = decode[notificationsResponse](t, rec.Body.Bytes())
	if len(box.Notifications) != 0 || box.Unread != 0 {
		t.Errorf("?unread returned %d of %d unread", len(box.Notifications), box.Unread)
	}
}
