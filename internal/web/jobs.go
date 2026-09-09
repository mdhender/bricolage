// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
)

// The job queue screen (PLAN.md M6's queue, M13's window onto it).
//
// Reading it needs read over the system subject and retrying needs publish
// over the same, which is the service's rule: the queue is not on a site and
// not in a category, so a site-scoped grant says nothing about the process
// that publishes that site's documents (DESIGN.md 12).
//
// There is deliberately no screen that creates a job. Nothing a person does is
// "enqueue a job": they publish something, and the operation that publishes it
// schedules the work.

// jobsPage is GET /jobs.
type jobsPage struct {
	Base

	Jobs   []jobRow
	Filter domain.JobFilter
}

type jobRow struct {
	Job domain.Job

	// Status is the word a person reads: pending, running, done, failed.
	// It is derived from the row rather than stored, because a lease is a
	// deadline that expires on its own and a status column would need
	// somebody to notice that it had.
	Status string

	// Retryable reports that the job has exhausted its attempts, which is
	// the only state a retry means anything in.
	Retryable bool
}

func (h *Handler) jobs(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	limit, err := intParam(r, "limit", 50)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	filter := domain.JobFilter{
		Kind:    r.URL.Query().Get("kind"),
		Pending: r.URL.Query().Get("pending") != "",
		Failed:  r.URL.Query().Get("failed") != "",
		Limit:   limit,
	}
	list, err := h.svc.Jobs(r.Context(), identity, filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	now := h.svc.Now()
	p := jobsPage{Base: h.base(r, identity, "Jobs"), Filter: filter}
	for _, j := range list {
		row := jobRow{Job: j}
		switch {
		case !j.CompletedAt.IsZero():
			row.Status = "done"
		case !j.FailedAt.IsZero():
			row.Status = "failed"
			row.Retryable = true
		case j.LeaseOwner != "" && now.Before(j.LeaseExpiresAt):
			row.Status = "running"
		default:
			row.Status = "pending"
		}
		p.Jobs = append(p.Jobs, row)
	}
	h.render(w, r, "jobs.gohtml", http.StatusOK, p)
}

// retryJob is POST /jobs/{uid}/retry: put an abandoned job back on the queue.
func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if _, err := h.svc.RetryJob(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/jobs", "job requeued")
}
