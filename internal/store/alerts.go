// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// All the SQL for alert rules, notifications, and the cursor between them
// (invariant 2, DESIGN.md 10, PLAN.md M12).
//
// The one thing in this file worth reading twice is Deliver. Evaluating a
// batch of events and recording what it produced is two writes -- the
// notification rows, and the cursor saying those events have been evaluated --
// and they are one transaction, because a crash between them is either an
// inbox that repeats itself or an alert nobody was ever told about. The cursor
// UPDATE names the value the batch was read at, so a second dispatcher racing
// this one loses the compare-and-swap and rolls its own batch back rather than
// delivering it twice.

// alertRuleSelect is the projection every rule read shares.
const alertRuleSelect = `
	SELECT id, uid, name, event_type, conditions, channel, target, active,
	       created_by, created_at, updated_at
	  FROM alert_rules`

// CreateAlertRule writes a rule and the event recording it, in one
// transaction (invariant 7).
func (db *DB) CreateAlertRule(ctx context.Context, r domain.AlertRule, event domain.Event) (domain.AlertRule, error) {
	conditions, err := r.ConditionsJSON()
	if err != nil {
		return domain.AlertRule{}, err
	}
	var out domain.AlertRule
	err = db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("creating alert rule %q", r.Name), `
			INSERT INTO alert_rules (uid, name, event_type, conditions, channel, target, active,
			                         created_by, created_at, updated_at)
			VALUES (:uid, :name, :event_type, :conditions, :channel, :target, :active,
			        :created_by, :created_at, :updated_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", r.UID)
				stmt.SetText(":name", r.Name)
				stmt.SetText(":event_type", r.EventType)
				stmt.SetText(":conditions", conditions)
				stmt.SetText(":channel", string(r.Channel))
				stmt.SetText(":target", r.Target)
				stmt.SetBool(":active", r.Active)
				if r.CreatedBy == 0 {
					stmt.SetNull(":created_by")
				} else {
					stmt.SetInt64(":created_by", r.CreatedBy)
				}
				stmt.SetText(":created_at", formatTime(r.CreatedAt))
				stmt.SetText(":updated_at", formatTime(r.UpdatedAt))
			}, nil)
		if err != nil {
			return err
		}
		id := conn.LastInsertRowID()

		event.SubjectKind = domain.SubjectAlertRule
		event.SubjectID = id
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return alertRuleByID(conn, id, &out)
	})
	return out, err
}

// UpdateAlertRule replaces a rule's configuration. Everything but the uid and
// who created it is rewritten, because a rule is small and a partial update
// that left one field behind would be a rule saying something nobody wrote.
func (db *DB) UpdateAlertRule(ctx context.Context, r domain.AlertRule, event domain.Event) (domain.AlertRule, error) {
	conditions, err := r.ConditionsJSON()
	if err != nil {
		return domain.AlertRule{}, err
	}
	var out domain.AlertRule
	err = db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("updating alert rule %q", r.UID), `
			UPDATE alert_rules
			   SET name = :name, event_type = :event_type, conditions = :conditions,
			       channel = :channel, target = :target, active = :active, updated_at = :updated_at
			 WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":name", r.Name)
				stmt.SetText(":event_type", r.EventType)
				stmt.SetText(":conditions", conditions)
				stmt.SetText(":channel", string(r.Channel))
				stmt.SetText(":target", r.Target)
				stmt.SetBool(":active", r.Active)
				stmt.SetText(":updated_at", formatTime(r.UpdatedAt))
				stmt.SetInt64(":id", r.ID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return notFound(fmt.Sprintf("alert rule %q", r.UID))
		}

		event.SubjectKind = domain.SubjectAlertRule
		event.SubjectID = r.ID
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		return alertRuleByID(conn, r.ID, &out)
	})
	return out, err
}

// DeleteAlertRule removes a rule.
//
// The notifications it produced survive it: notifications.rule_id is
// ON DELETE SET NULL (DESIGN.md 10), because deleting a rule must not delete
// what it already told somebody. The event is written against the id before
// the row goes, so a deleted rule still has a history -- the same order
// DeleteCategory uses and for the same reason.
func (db *DB) DeleteAlertRule(ctx context.Context, id int64, event domain.Event) error {
	return db.Tx(ctx, func(conn *sqlite.Conn) error {
		event.SubjectKind = domain.SubjectAlertRule
		event.SubjectID = id
		if _, err := recordEvent(conn, event); err != nil {
			return err
		}
		err := run(conn, fmt.Sprintf("deleting alert rule %d", id),
			`DELETE FROM alert_rules WHERE id = :id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) }, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return notFound(fmt.Sprintf("alert rule %d", id))
		}
		return nil
	})
}

// AlertRuleByUID reads one rule by the identifier the API speaks
// (invariant 10).
func (db *DB) AlertRuleByUID(ctx context.Context, uid string) (domain.AlertRule, error) {
	var out domain.AlertRule
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("alert rule %q", uid),
			alertRuleSelect+` WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanAlertRule(stmt)
				return err
			})
	})
	return out, err
}

// ListAlertRules returns every rule, oldest first, active and inactive alike.
// A rule that is off is still configuration somebody has to be able to see.
func (db *DB) ListAlertRules(ctx context.Context) ([]domain.AlertRule, error) {
	return db.alertRules(ctx, alertRuleSelect+` ORDER BY id`, nil)
}

// ActiveAlertRules returns the rules the dispatcher evaluates.
//
// Every active rule, in one read per batch rather than one per event: an
// installation has tens of rules and a batch has tens of events, and the join
// that would filter by the batch's event types is a join over a list SQLite
// would have to be handed as parameters. alert_rules_event, from migration
// 0012, is what the in-memory grouping stands in for.
func (db *DB) ActiveAlertRules(ctx context.Context) ([]domain.AlertRule, error) {
	return db.alertRules(ctx, alertRuleSelect+` WHERE active = 1 ORDER BY id`, nil)
}

func (db *DB) alertRules(ctx context.Context, query string, bind func(*sqlite.Stmt)) ([]domain.AlertRule, error) {
	var out []domain.AlertRule
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing alert rules", query, bind, func(stmt *sqlite.Stmt) error {
			r, err := scanAlertRule(stmt)
			if err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func alertRuleByID(conn *sqlite.Conn, id int64, r *domain.AlertRule) error {
	return one(conn, fmt.Sprintf("alert rule %d", id),
		alertRuleSelect+` WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*r, err = scanAlertRule(stmt)
			return err
		})
}

func scanAlertRule(stmt *sqlite.Stmt) (domain.AlertRule, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("alert rule %d: created_at: %w", id, err)
	}
	updated, err := parseTime(stmt.GetText("updated_at"))
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("alert rule %d: updated_at: %w", id, err)
	}
	conditions, err := domain.ParseConditions(stmt.GetText("conditions"))
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("alert rule %d: %w", id, err)
	}
	r := domain.AlertRule{
		ID:         id,
		UID:        stmt.GetText("uid"),
		Name:       stmt.GetText("name"),
		EventType:  stmt.GetText("event_type"),
		Conditions: conditions,
		Channel:    domain.AlertChannel(stmt.GetText("channel")),
		Target:     stmt.GetText("target"),
		Active:     stmt.GetBool("active"),
		CreatedAt:  created,
		UpdatedAt:  updated,
	}
	if v := nullInt64(stmt, "created_by"); v != nil {
		r.CreatedBy = *v
	}
	return r, nil
}

// PendingAlertEvents reads the events the dispatcher has not evaluated yet,
// oldest first, at most limit of them.
//
// Oldest first and by id, which is the order they were committed in. A batch
// smaller than limit means the dispatcher has caught up; the caller polls
// again rather than blocking, for the reason the job workers poll
// (internal/jobs/pool.go).
func (db *DB) PendingAlertEvents(ctx context.Context, limit int) (domain.AlertBatch, error) {
	if limit <= 0 {
		limit = 100
	}
	var b domain.AlertBatch
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		if err := alertCursor(conn, &b.Cursor); err != nil {
			return err
		}
		return run(conn, "reading events for alert rules", `
			SELECT id, type, actor_id, subject_kind, subject_id, payload, occurred_at
			  FROM events
			 WHERE id > :cursor
			 ORDER BY id
			 LIMIT :limit`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":cursor", b.Cursor)
				stmt.SetInt64(":limit", int64(limit))
			},
			func(stmt *sqlite.Stmt) error {
				e, err := scanEvent(stmt)
				if err != nil {
					return err
				}
				b.Events = append(b.Events, e)
				return nil
			})
	})
	return b, err
}

// alertCursor reads the one row of alert_cursor on a connection the caller
// holds.
func alertCursor(conn *sqlite.Conn, into *int64) error {
	return one(conn, "the alert cursor",
		`SELECT last_event_id FROM alert_cursor WHERE id = 1`, nil,
		func(stmt *sqlite.Stmt) error {
			*into = stmt.GetInt64("last_event_id")
			return nil
		})
}

// Deliver records a batch's notifications and advances the cursor past it, in
// one transaction (PLAN.md M12).
//
// It reports how many rows it wrote and whether the cursor moved. A false
// means another dispatcher evaluated the same batch first: the transaction
// rolled back, nothing was written, and the caller reads a fresh batch rather
// than retrying this one.
//
// A row that collides with notifications_once is skipped rather than failing
// the batch. The unique index is there to make "one notification per person
// per event per rule" a property of the schema, and two rules that resolve to
// the same person through different targets are not an error to report to
// anybody -- they are one line in an inbox, which is what the index says.
func (db *DB) Deliver(ctx context.Context, cursor, next int64, notes []domain.NewNotification, now time.Time) (int, bool, error) {
	var (
		written int
		moved   bool
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		written = 0
		for _, n := range notes {
			err := run(conn, fmt.Sprintf("notifying user %d of event %d", n.UserID, n.EventID), `
				INSERT OR IGNORE INTO notifications (uid, user_id, event_id, rule_id, created_at)
				VALUES (:uid, :user_id, :event_id, :rule_id, :created_at)`,
				func(stmt *sqlite.Stmt) {
					stmt.SetText(":uid", n.UID)
					stmt.SetInt64(":user_id", n.UserID)
					stmt.SetInt64(":event_id", n.EventID)
					stmt.SetInt64(":rule_id", n.RuleID)
					stmt.SetText(":created_at", formatTime(n.CreatedAt))
				}, nil)
			if err != nil {
				return err
			}
			written += conn.Changes()
		}

		// The compare-and-swap. It names the value the batch was read at, so
		// two dispatchers cannot both conclude they delivered it.
		err := run(conn, "advancing the alert cursor", `
			UPDATE alert_cursor SET last_event_id = :next, updated_at = :now
			 WHERE id = 1 AND last_event_id = :cursor`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":next", next)
				stmt.SetText(":now", formatTime(now))
				stmt.SetInt64(":cursor", cursor)
			}, nil)
		if err != nil {
			return err
		}
		moved = conn.Changes() == 1
		if !moved {
			// Rolling back discards the notifications too, which is the
			// point: whoever won the swap is delivering this batch.
			return errCursorMoved
		}
		return nil
	})
	if errors.Is(err, errCursorMoved) {
		return 0, false, nil
	}
	return written, moved, err
}

// errCursorMoved rolls a batch back when another dispatcher got there first.
// It never leaves this package: Deliver reports it as "not moved".
var errCursorMoved = errors.New("the alert cursor moved under this batch")

// notificationSelect is the projection every notification read shares.
//
// It joins the event, because a notification with no news in it is a row that
// makes a person go and look something up, and it LEFT JOINs the rule, because
// a rule may have been deleted since (notifications.rule_id is
// ON DELETE SET NULL) and a notification that vanished from an inbox when
// somebody tidied up the rules would be worse than one that says "a rule that
// no longer exists".
const notificationSelect = `
	SELECT n.id, n.uid, n.user_id, n.event_id, n.rule_id, n.read_at, n.created_at,
	       e.type AS event_type, e.subject_kind, e.subject_id, e.payload, e.occurred_at,
	       r.uid AS rule_uid, r.name AS rule_name
	  FROM notifications n
	  JOIN events e ON e.id = n.event_id
	  LEFT JOIN alert_rules r ON r.id = n.rule_id`

// NotificationsForUser returns somebody's inbox, newest first.
func (db *DB) NotificationsForUser(ctx context.Context, userID int64, unreadOnly bool, limit int) ([]domain.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	where := ` WHERE n.user_id = :user_id`
	if unreadOnly {
		where += ` AND n.read_at IS NULL`
	}
	var out []domain.Notification
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("notifications for user %d", userID),
			notificationSelect+where+` ORDER BY n.id DESC LIMIT :limit`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":user_id", userID)
				stmt.SetInt64(":limit", int64(limit))
			},
			func(stmt *sqlite.Stmt) error {
				n, err := scanNotification(stmt)
				if err != nil {
					return err
				}
				out = append(out, n)
				return nil
			})
	})
	return out, err
}

// CountUnreadNotifications is the number beside the bell.
func (db *DB) CountUnreadNotifications(ctx context.Context, userID int64) (int, error) {
	var n int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("unread notifications for user %d", userID), `
			SELECT COUNT(*) AS n FROM notifications
			 WHERE user_id = :user_id AND read_at IS NULL`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":user_id", userID) },
			func(stmt *sqlite.Stmt) error {
				n = int(stmt.GetInt64("n"))
				return nil
			})
	})
	return n, err
}

// MarkNotificationRead marks one of a person's own notifications read and
// reports whether this call is what marked it.
//
// The UPDATE names the user as well as the uid, so somebody else's
// notification is not found rather than refused: an inbox is not a resource
// with permissions on it, it is a person's, and a route that distinguished
// "not yours" from "no such thing" would be a route that enumerated other
// people's mail.
//
// Marking a notification that is already read changes nothing and writes
// nothing, which is the answer ResolveComment gives to a second resolution and
// for the same reason.
func (db *DB) MarkNotificationRead(ctx context.Context, uid string, userID int64, now time.Time) (domain.Notification, bool, error) {
	var (
		out    domain.Notification
		marked bool
	)
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("reading notification %q", uid), `
			UPDATE notifications SET read_at = :now
			 WHERE uid = :uid AND user_id = :user_id AND read_at IS NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(now))
				stmt.SetText(":uid", uid)
				stmt.SetInt64(":user_id", userID)
			}, nil)
		if err != nil {
			return err
		}
		marked = conn.Changes() == 1
		return one(conn, fmt.Sprintf("notification %q", uid),
			notificationSelect+` WHERE n.uid = :uid AND n.user_id = :user_id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", uid)
				stmt.SetInt64(":user_id", userID)
			},
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanNotification(stmt)
				return err
			})
	})
	return out, marked, err
}

func scanNotification(stmt *sqlite.Stmt) (domain.Notification, error) {
	id := stmt.GetInt64("id")
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Notification{}, fmt.Errorf("notification %d: created_at: %w", id, err)
	}
	occurred, err := parseTime(stmt.GetText("occurred_at"))
	if err != nil {
		return domain.Notification{}, fmt.Errorf("notification %d: occurred_at: %w", id, err)
	}
	n := domain.Notification{
		ID:          id,
		UID:         stmt.GetText("uid"),
		UserID:      stmt.GetInt64("user_id"),
		EventID:     stmt.GetInt64("event_id"),
		CreatedAt:   created,
		EventType:   stmt.GetText("event_type"),
		SubjectKind: stmt.GetText("subject_kind"),
		SubjectID:   stmt.GetInt64("subject_id"),
		OccurredAt:  occurred,
	}
	if v := nullInt64(stmt, "rule_id"); v != nil {
		n.RuleID = *v
	}
	if v := nullText(stmt, "rule_uid"); v != nil {
		n.RuleUID = *v
	}
	if v := nullText(stmt, "rule_name"); v != nil {
		n.RuleName = *v
	}
	if v := nullText(stmt, "read_at"); v != nil {
		at, err := parseTime(*v)
		if err != nil {
			return domain.Notification{}, fmt.Errorf("notification %d: read_at: %w", id, err)
		}
		n.ReadAt = at
	}
	if err := unmarshalPayload(stmt.GetText("payload"), &n.Payload); err != nil {
		return domain.Notification{}, fmt.Errorf("notification %d: payload: %w", id, err)
	}
	return n, nil
}

// UsersWithRole returns everybody holding a role, oldest account first.
//
// It is what a "role:" alert target resolves to. A role that names nobody is
// not an error: a rule notifying an empty desk notifies nobody, and refusing
// it would mean an alert stops working the day the last person leaves the
// team, which is the day it matters most.
//
// The password hash is deliberately not in the projection. It leaves the store
// on its way into the verifier and nowhere else (DESIGN.md 14), and a list of
// recipients is not that.
func (db *DB) UsersWithRole(ctx context.Context, slug string) ([]domain.User, error) {
	var out []domain.User
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("users holding role %q", slug), `
			SELECT u.id AS id, u.uid AS uid, u.email AS email, u.name AS name,
			       u.active AS active, u.created_at AS created_at
			  FROM users u
			  JOIN user_roles ur ON ur.user_id = u.id
			  JOIN roles r       ON r.id = ur.role_id
			 WHERE r.slug = :slug
			 ORDER BY u.id`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":slug", slug) },
			func(stmt *sqlite.Stmt) error {
				created, err := parseTime(stmt.GetText("created_at"))
				if err != nil {
					return fmt.Errorf("user %d: created_at: %w", stmt.GetInt64("id"), err)
				}
				out = append(out, domain.User{
					ID:        stmt.GetInt64("id"),
					UID:       stmt.GetText("uid"),
					Email:     stmt.GetText("email"),
					Name:      stmt.GetText("name"),
					Active:    stmt.GetBool("active"),
					CreatedAt: created,
				})
				return nil
			})
	})
	return out, err
}
