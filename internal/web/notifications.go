// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// The inbox (PLAN.md M12's notifications, M13's screen).
//
// It is the caller's own and nobody else's. There is deliberately no screen
// here that shows another person's, and no privilege that would open one:
// reading somebody's inbox would report what the rules are watching them do
// (DESIGN.md 12).

// inboxPage is GET /notifications.
type inboxPage struct {
	Base

	Notifications []notificationRow
	UnreadOnly    bool
	Total         int
}

type notificationRow struct {
	Notification domain.Notification

	// Name is the event type's display name, from the vocabulary in
	// internal/events rather than from a SELECT (DESIGN.md 10).
	Name string
	Read bool
}

func (h *Handler) notifications(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	unreadOnly := r.URL.Query().Get("unread") != ""
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	inbox, err := h.svc.Notifications(r.Context(), identity, unreadOnly, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := inboxPage{
		Base:       h.base(r, identity, "Notifications"),
		UnreadOnly: unreadOnly,
		Total:      len(inbox.Notifications),
	}
	p.Unread = inbox.Unread
	for _, n := range inbox.Notifications {
		p.Notifications = append(p.Notifications, notificationRow{
			Notification: n,
			Name:         events.Name(n.EventType),
			Read:         !n.ReadAt.IsZero(),
		})
	}
	h.render(w, r, "notifications.gohtml", http.StatusOK, p)
}

// readNotification is POST /notifications/{uid}/read.
//
// A notification that is not the caller's own is not found rather than
// refused, which is the service's rule and the reason there is no privilege
// involved: somebody else's uid must not be distinguishable from one that does
// not exist.
func (h *Handler) readNotification(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	n, changed, err := h.svc.MarkNotificationRead(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if hx(r) {
		h.partial(w, r, "notification", http.StatusOK, notificationRow{
			Notification: n,
			Name:         events.Name(n.EventType),
			Read:         true,
		})
		return
	}
	notice := "already read"
	if changed {
		notice = "marked read"
	}
	redirect(w, r, "/notifications", notice)
}
