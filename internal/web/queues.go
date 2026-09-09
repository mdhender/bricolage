// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The dashboard and the saved queues (DESIGN.md 14, "Saved queues are
// configuration, not schema").
//
// A queue is a named question about the document table, resolved per request:
// "mine" means whoever is asking, which is what lets one definition serve
// every editor. There is deliberately no second query behind a queue -- the
// service turns the definition into the same domain.DocumentFilter a filtered
// list uses -- so a queue that answered differently from the same question
// asked directly cannot happen.

// queueCounts asks every saved queue its own question.
//
// A queue that refuses carries its refusal rather than failing the page: the
// definitions are configuration, and one of them naming a state this
// installation does not have is a fact about that queue and not about the
// screen.
func (h *Handler) queueCounts(r *http.Request, identity domain.Identity) []queueCount {
	var out []queueCount
	for _, q := range h.svc.Queues() {
		entry := queueCount{Queue: q}
		if _, docs, err := h.svc.Queue(r.Context(), identity, q.Slug); err != nil {
			entry.Err = err.Error()
		} else {
			entry.Count = len(docs)
		}
		out = append(out, entry)
	}
	return out
}

// dashboardPage is GET /.
type dashboardPage struct {
	Base

	Queues []queueCount
	Recent []documentRow
	Inbox  []domain.Notification
}

// queueCount is one saved queue and how much is in it.
type queueCount struct {
	Queue config.Queue
	Count int
	Err   string
}

// dashboard is the first screen: what is waiting, and what has happened.
func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	p := dashboardPage{Base: h.base(r, identity, "Dashboard"), Queues: h.queueCounts(r, identity)}

	// The recent list is the same question "earl doc list" asks with no
	// filters, bounded to what fits on a dashboard.
	filter, err := h.svc.ResolveFilter(r.Context(), identity, service.FilterRequest{Limit: 10})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	docs, err := h.svc.ListDocuments(r.Context(), identity, filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Recent = h.rows(r, docs)

	if inbox, err := h.svc.Notifications(r.Context(), identity, true, 5); err == nil {
		p.Inbox = inbox.Notifications
	}

	h.render(w, r, "dashboard.gohtml", http.StatusOK, p)
}

// queuesPage is GET /queues.
type queuesPage struct {
	Base

	Queues []queueCount
}

func (h *Handler) queues(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	h.render(w, r, "queues.gohtml", http.StatusOK, queuesPage{
		Base:   h.base(r, identity, "Queues"),
		Queues: h.queueCounts(r, identity),
	})
}

// queuePage is GET /queues/{slug}.
type queuePage struct {
	Base

	Queue     config.Queue
	Documents []documentRow
}

// queue draws one saved queue. A slug this binary does not know is a 404 from
// the service, which is the honest answer: the definitions are configuration,
// and this server's set is what it was started with.
func (h *Handler) queue(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	q, docs, err := h.svc.Queue(r.Context(), identity, r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "queue.gohtml", http.StatusOK, queuePage{
		Base:      h.base(r, identity, q.Name),
		Queue:     q,
		Documents: h.rows(r, docs),
	})
}
