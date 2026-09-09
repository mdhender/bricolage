// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/store"
)

// The queue's use cases (PLAN.md M6). What is under test here is who may look
// and who may press the button, because the mechanics are the store's and the
// worker loop's.

// scopedTo builds an identity whose grant names one site, which is the shape
// that must NOT reach a system-wide resource.
func (h *harness) scopedTo(t *testing.T, email string, p domain.Privilege) domain.Identity {
	t.Helper()
	return h.userWithGrant(t, email, "correct horse battery", domain.Grant{
		Privilege: p,
		Scope:     domain.Scope{SiteID: &h.siteID},
	})
}

// failOne enqueues a job and drives it to abandoned, which is the only state a
// retry is defined on.
func (h *harness) failOne(t *testing.T, kind string) domain.Job {
	t.Helper()
	job, err := h.EnqueueJob(t.Context(), domain.NewJob{Kind: kind, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	// Claim until this job comes up rather than failing whatever the queue
	// happens to hand back first: a test that ran after one which left a job
	// pending would otherwise fail that one. Everything claimed on the way
	// keeps its lease, which takes it out of the pool without making it
	// claimable again.
	q := h.JobQueue()
	for {
		claimed, ok, err := q.Claim(t.Context(), "worker-1")
		if err != nil || !ok {
			t.Fatalf("job %s was never claimed: %v, %v", job.UID, ok, err)
		}
		if claimed.UID != job.UID {
			continue
		}
		if err := q.Fail(t.Context(), claimed, "worker-1", errors.New("it broke")); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		break
	}

	failed, err := q.Job(t.Context(), job.UID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if failed.FailedAt.IsZero() {
		t.Fatalf("the job did not reach failed: %+v", failed)
	}
	return failed
}

// TestJobsNeedsAGlobalGrant is the rule this file exists for. The queue is not
// on a site and not in a category, so a site-scoped grant says nothing about
// it and must not reach it.
func TestJobsNeedsAGlobalGrant(t *testing.T) {
	h := newHarness(t)
	if _, err := h.EnqueueJob(t.Context(), domain.NewJob{Kind: "noop"}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	t.Run("a global reader sees it", func(t *testing.T) {
		reader := h.admin(t, "reader@example.com", "correct horse battery", domain.Read)
		got, err := h.Jobs(t.Context(), reader, domain.JobFilter{})
		if err != nil {
			t.Fatalf("Jobs: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("a global reader saw %d jobs, want 1", len(got))
		}
	})

	t.Run("a site-scoped editor does not", func(t *testing.T) {
		// Publish over one site is a great deal of privilege, and none of it
		// is over the process that publishes.
		scoped := h.scopedTo(t, "site-editor@example.com", domain.Publish)
		_, err := h.Jobs(t.Context(), scoped, domain.JobFilter{})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Jobs for a site-scoped grant = %v, want not found", err)
		}
	})

	t.Run("somebody with no grant does not", func(t *testing.T) {
		nobody := h.admin(t, "nobody@example.com", "correct horse battery", domain.NoPrivilege)
		if _, err := h.Jobs(t.Context(), nobody, domain.JobFilter{}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Jobs for a user with no grant = %v, want not found", err)
		}
	})
}

// TestRetryJobNeedsPublish is the privilege on the button. A job's payload is
// whatever the operation that enqueued it decided, and running it again runs
// that operation again; requiring less would make the retry a way around the
// privilege the original operation needed.
func TestRetryJobNeedsPublish(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name      string
		email     string
		privilege domain.Privilege
		wantErr   error
	}{
		{"a reader may look but not retry", "reader", domain.Read, domain.ErrForbidden},
		{"an editor may not either", "editor", domain.Edit, domain.ErrForbidden},
		{"a creator may not either", "creator", domain.Create, domain.ErrForbidden},
		{"publish may", "publisher", domain.Publish, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := h.failOne(t, "noop")
			actor := h.admin(t, tc.email+"@example.com", "correct horse battery", tc.privilege)

			got, err := h.RetryJob(t.Context(), actor, job.UID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("RetryJob = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RetryJob: %v", err)
			}
			if !got.FailedAt.IsZero() || got.Attempts != 0 {
				t.Errorf("the retried job is not pending: %+v", got)
			}

			// The event names who pressed it, and carries the failure the
			// retry cleared out of the columns (invariant 7).
			retries := h.eventsOfType(t, events.JobRetried)
			if len(retries) == 0 {
				t.Fatal("no job.retried event was written")
			}
			e := retries[0]
			if e.ActorID != actor.User.ID {
				t.Errorf("the event's actor is %d, want %d", e.ActorID, actor.User.ID)
			}
			if e.SubjectKind != domain.SubjectJob || e.SubjectID != job.ID {
				t.Errorf("the event's subject is %s %d, want %s %d",
					e.SubjectKind, e.SubjectID, domain.SubjectJob, job.ID)
			}
			if e.Payload["last_error"] != "it broke" {
				t.Errorf("the event does not carry the failure it was retried out of: %v", e.Payload)
			}
		})
	}
}

// TestRetryRefusesAJobThatHasNotFailed keeps "retry" from being a way to reset
// something a worker is running.
func TestRetryRefusesAJobThatHasNotFailed(t *testing.T) {
	h := newHarness(t)
	admin := h.admin(t, "admin@example.com", "correct horse battery", domain.Publish)

	job, err := h.EnqueueJob(t.Context(), domain.NewJob{Kind: "noop"})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if _, err := h.RetryJob(t.Context(), admin, job.UID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("retrying a pending job = %v, want a conflict", err)
	}

	// And a uid nobody holds is a 404 rather than a silent success.
	if _, err := h.RetryJob(t.Context(), admin, "nosuchjobuid0000000000000"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("retrying an unknown uid = %v, want not found", err)
	}
}

// TestEnqueueWritesItsEvent is invariant 7 for the one operation that creates
// a job, and the check that the payload -- which is the handler's argument and
// may name anything -- stays out of the audit log (DESIGN.md 14).
func TestEnqueueWritesItsEvent(t *testing.T) {
	h := newHarness(t)

	job, err := h.EnqueueJob(t.Context(), domain.NewJob{
		Kind:     "noop",
		Priority: domain.PriorityBulk,
		Payload:  `{"secret":"do not log me"}`,
	})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	enqueued := h.eventsOfType(t, events.JobEnqueued)
	if len(enqueued) != 1 {
		t.Fatalf("%d job.enqueued events, want 1", len(enqueued))
	}
	e := enqueued[0]
	if e.SubjectKind != domain.SubjectJob || e.SubjectID != job.ID {
		t.Errorf("subject = %s %d, want %s %d", e.SubjectKind, e.SubjectID, domain.SubjectJob, job.ID)
	}
	if e.Payload["kind"] != "noop" {
		t.Errorf("the event does not name the kind: %v", e.Payload)
	}
	if _, ok := e.Payload["payload"]; ok {
		t.Errorf("the event carries the job's payload: %v", e.Payload)
	}
	for k, v := range e.Payload {
		if s, ok := v.(string); ok && s == "do not log me" {
			t.Errorf("the event leaked the payload through %q", k)
		}
	}
}

// TestJobsFilters is the listing the API and earl put in front of a person.
func TestJobsFilters(t *testing.T) {
	h := newHarness(t)
	admin := h.admin(t, "admin@example.com", "correct horse battery", domain.Publish)

	failed := h.failOne(t, "noop")
	pending, err := h.EnqueueJob(t.Context(), domain.NewJob{Kind: "other"})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	for name, tc := range map[string]struct {
		filter domain.JobFilter
		want   []string
	}{
		"everything": {domain.JobFilter{}, []string{pending.UID, failed.UID}},
		"pending":    {domain.JobFilter{Pending: true}, []string{pending.UID}},
		"failed":     {domain.JobFilter{Failed: true}, []string{failed.UID}},
		"by kind":    {domain.JobFilter{Kind: "other"}, []string{pending.UID}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := h.Jobs(t.Context(), admin, tc.filter)
			if err != nil {
				t.Fatalf("Jobs(%+v): %v", tc.filter, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d jobs, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, uid := range tc.want {
				if got[i].UID != uid {
					t.Errorf("job %d is %s, want %s", i, got[i].UID, uid)
				}
			}
		})
	}

	t.Run("a contradiction is refused", func(t *testing.T) {
		_, err := h.Jobs(t.Context(), admin, domain.JobFilter{Pending: true, Failed: true})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("pending and failed together = %v, want invalid", err)
		}
	})
}

// TestTheServiceQueueIsTheOneTheStoreSees is that the service and the worker
// pool share one queue over one clock. Two would be two answers to "when does
// this lease expire".
func TestTheServiceQueueIsTheOneTheStoreSees(t *testing.T) {
	h := newHarness(t)
	if h.JobQueue() == nil {
		t.Fatal("the service has no job queue")
	}

	job, err := h.EnqueueJob(t.Context(), domain.NewJob{Kind: "noop"})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	// Enqueued through the service, claimed through the store: one table.
	claimed, ok, err := h.db.ClaimJob(t.Context(), store.ClaimRequest{
		Owner: "worker-1", Now: h.Now(), Lease: time.Minute,
	})
	if err != nil || !ok {
		t.Fatalf("ClaimJob = %v, %v", ok, err)
	}
	if claimed.UID != job.UID {
		t.Errorf("claimed %s, want %s", claimed.UID, job.UID)
	}

	// The service's clock is the queue's clock, so advancing one advances
	// both.
	h.clock.Advance(2 * time.Minute)
	if h.JobQueue().Now() != h.Now() {
		t.Errorf("the queue reads %v and the service reads %v", h.JobQueue().Now(), h.Now())
	}
}
