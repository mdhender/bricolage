// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// The job routes (DESIGN.md 12, PLAN.md M6).
//
// There are two, and there is deliberately no third. DESIGN.md 12 lists
// GET /api/v1/jobs and POST /api/v1/jobs/{uid}/retry and nothing that creates
// one, because nothing a person does is "enqueue a job": they publish
// something, and the operation that publishes it schedules the work. A route
// that accepted a kind and a payload would be a way to run any handler in the
// binary with arguments the client chose.
//
// The path speaks uid. DESIGN.md 12 writes the retry route as
// /api/v1/jobs/{id}/retry, and invariant 10 says the API speaks uid only:
// internal integer primary keys never appear in a URL, a JSON body, or
// user-facing output. An invariant outranks a path spelled in an example, so
// jobs carry a uid like every other externally addressable row and the design
// document has been corrected to match.

// jobResponse is one job as the API renders it.
//
// The payload is not in it. A payload is the handler's argument -- a version
// id today, anything a future kind decides tomorrow -- and a queue listing is
// not the place to hand it back out; what a client needs in order to act is
// which job, of what kind, in what state, and why it stopped.
type jobResponse struct {
	UID  string `json:"uid"`
	Kind string `json:"kind"`

	// Status is derived from the row against the server's clock, so that a
	// client does not have to work out that a lease in the past means the job
	// is not running.
	Status   string `json:"status"`
	Priority int    `json:"priority"`

	ScheduledFor time.Time `json:"scheduled_for"`

	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`

	// LastError is the most recent attempt's message. It survives a retry,
	// because "it failed like this and then worked" is worth keeping.
	LastError string `json:"last_error,omitempty"`

	// Worker names the holder of a live lease, and LeaseExpiresAt says until
	// when. Both are absent when nobody is running it.
	Worker         string     `json:"worker,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`

	CompletedAt *time.Time `json:"completed_at,omitempty"`
	FailedAt    *time.Time `json:"failed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func newJobResponse(j domain.Job, now time.Time) jobResponse {
	out := jobResponse{
		UID:          j.UID,
		Kind:         j.Kind,
		Status:       string(j.Status(now)),
		Priority:     j.Priority,
		ScheduledFor: j.ScheduledFor,
		Attempts:     j.Attempts,
		MaxAttempts:  j.MaxAttempts,
		LastError:    j.LastError,
		CreatedAt:    j.CreatedAt,
	}
	// The lease is rendered only while it is one. An expired lease is not a
	// lease, and showing the owner of one is how a client draws "running" over
	// a job nobody is running.
	if j.Leased(now) {
		out.Worker = j.LeaseOwner
		out.LeaseExpiresAt = timeOrNil(j.LeaseExpiresAt)
	}
	out.CompletedAt = timeOrNil(j.CompletedAt)
	out.FailedAt = timeOrNil(j.FailedAt)
	return out
}

// timeOrNil renders the zero time as absent rather than as the year 1.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// listJobs is GET /api/v1/jobs, with ?pending, ?failed, ?kind, and ?limit.
func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	pending, err := boolParam(r, "pending")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	failed, err := boolParam(r, "failed")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	limit, err := intParam(r, "limit", domain.DefaultJobListLimit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	filter := domain.JobFilter{
		Kind:    r.URL.Query().Get("kind"),
		Pending: pending,
		Failed:  failed,
		Limit:   limit,
	}
	list, err := h.svc.Jobs(r.Context(), identity, filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	now := h.svc.Now()
	out := make([]jobResponse, 0, len(list))
	for _, j := range list {
		out = append(out, newJobResponse(j, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":  out,
		"count": len(out),
		"asks":  filter.Normalize().Describe(),
	})
}

// retryJob is POST /api/v1/jobs/{uid}/retry.
//
// POST rather than PATCH: retrying is an act performed on the job, not a field
// being set on it, and there is no route here that writes a job's columns
// directly. It answers with the job as it now stands -- pending, attempts back
// at zero -- so that a client can show the result rather than asking again.
func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	job, err := h.svc.RetryJob(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newJobResponse(job, h.svc.Now()))
}
