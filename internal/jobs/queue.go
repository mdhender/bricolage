// Copyright (c) 2026 Michael D Henderson.

package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// DefaultLease is how long a claim is good for. It is short on purpose: it
// bounds how long a job stays invisible after the worker running it dies, and
// a job that needs longer says so by extending its lease rather than by
// everything else waiting.
const DefaultLease = 60 * time.Second

// Queue is the job queue as everything above the store sees it.
//
// It holds a clock rather than reading one, which is what makes lease expiry,
// scheduling, and backoff testable at an arbitrary instant (invariant 3). It
// holds no state of its own: two Queues over the same database are the same
// queue, which is what lets the workers and the API use one each without
// coordinating.
type Queue struct {
	db    *store.DB
	clock clock.Clock

	// lease is how long a claim lasts. It is configuration rather than a
	// column, because it is a property of this process's confidence in
	// itself rather than of the work.
	lease time.Duration
}

// NewQueue builds a queue over an open database.
func NewQueue(db *store.DB, c clock.Clock, lease time.Duration) (*Queue, error) {
	if db == nil {
		return nil, fmt.Errorf("jobs: no database")
	}
	if c == nil {
		return nil, fmt.Errorf("jobs: no clock; every component that needs the time is given one (invariant 3)")
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	return &Queue{db: db, clock: c, lease: lease}, nil
}

// Lease is how long a claim from this queue lasts.
func (q *Queue) Lease() time.Duration { return q.lease }

// Now reads the injected clock. Nothing in this package calls time.Now
// (invariant 3).
func (q *Queue) Now() time.Time { return q.clock.Now().UTC() }

// Enqueue schedules a job and writes the event that says so.
//
// The uid is minted here rather than in the store, because a ULID encodes an
// instant and this is the layer that holds the clock. The payload is the
// handler's argument and never appears in the event: a publish job's payload
// names a version, a future kind's may name anything, and an audit log is not
// where either belongs (DESIGN.md 14).
func (q *Queue) Enqueue(ctx context.Context, n domain.NewJob) (domain.Job, error) {
	now := q.Now()
	n = n.Normalize(now)
	if err := n.Validate(); err != nil {
		return domain.Job{}, err
	}

	uid, err := ids.New(now)
	if err != nil {
		return domain.Job{}, err
	}

	return q.db.EnqueueJob(ctx, store.NewJob{
		UID: uid,
		Job: n,
		Event: domain.Event{
			Type:    events.JobEnqueued,
			ActorID: n.CreatedBy,
			Payload: map[string]any{
				"uid":           uid,
				"kind":          n.Kind,
				"priority":      n.Priority,
				"scheduled_for": n.ScheduledFor,
				"max_attempts":  n.MaxAttempts,
			},
			OccurredAt: now,
		},
	})
}

// Claim takes at most one job for worker, or reports that nothing is ready.
//
// "Nothing is ready" is the ordinary state of an idle queue and not an error,
// which is why it is a bool rather than a sentinel: a caller that had to
// distinguish errors.Is(err, ErrNoWork) from a real failure would eventually
// stop distinguishing.
func (q *Queue) Claim(ctx context.Context, worker string) (domain.Job, bool, error) {
	return q.db.ClaimJob(ctx, store.ClaimRequest{
		Owner: worker,
		Now:   q.Now(),
		Lease: q.lease,
	})
}

// Complete marks a job done. The worker must still hold the lease.
func (q *Queue) Complete(ctx context.Context, job domain.Job, worker string) error {
	now := q.Now()
	_, err := q.db.CompleteJob(ctx, store.JobOutcome{
		JobID: job.ID,
		Owner: worker,
		Now:   now,
		Event: domain.Event{
			Type: events.JobCompleted,
			Payload: map[string]any{
				"uid":      job.UID,
				"kind":     job.Kind,
				"worker":   worker,
				"attempts": job.Attempts,
			},
			OccurredAt: now,
		},
	})
	return err
}

// Fail records a failed attempt and decides whether there will be another.
//
// The decision is made here rather than in the store because it is a rule:
// attempts is incremented by the claim, so a handler that has just returned an
// error has already spent the attempt, and "was that the last one" is
// domain.Job.Exhausted. The store writes whichever outcome this names, in one
// statement, so a crash cannot leave a job that is neither retried nor
// abandoned.
//
// A retry is scheduled with backoff rather than immediately
// (domain.RetryDelay). Retrying at once is how a queue turns one outage into
// five failures in the same second and a job somebody has to find by hand.
func (q *Queue) Fail(ctx context.Context, job domain.Job, worker string, cause error) error {
	now := q.Now()
	message := cause.Error()

	// job carries the attempt count as the claim left it, which already
	// includes the attempt that has just failed.
	exhausted := job.Exhausted()
	retryAt := now.Add(domain.RetryDelay(job.Attempts))

	payload := map[string]any{
		"uid":          job.UID,
		"kind":         job.Kind,
		"worker":       worker,
		"attempt":      job.Attempts,
		"max_attempts": job.MaxAttempts,
		"error":        message,
	}
	eventType := events.JobFailed
	if exhausted {
		eventType = events.JobAbandoned
	} else {
		payload["retry_at"] = retryAt
	}

	_, err := q.db.FailJob(ctx, store.JobOutcome{
		JobID: job.ID,
		Owner: worker,
		Now:   now,
		Event: domain.Event{
			Type:       eventType,
			Payload:    payload,
			OccurredAt: now,
		},
	}, message, retryAt)
	return err
}

// Release gives an in-flight job back without recording an outcome.
//
// It is what a worker does when the process is shutting down under it
// (PLAN.md M6 acceptance 6). The job did not succeed and it did not fail; the
// honest record is that nobody is holding it, so the next worker to look finds
// it at once rather than after a lease.
//
// It writes no event, because nothing happened to the job: the attempt is
// already counted, and "a process stopped" is a fact about the process.
func (q *Queue) Release(ctx context.Context, job domain.Job, worker string) error {
	_, err := q.db.ReleaseJob(ctx, store.JobOutcome{
		JobID: job.ID,
		Owner: worker,
		Now:   q.Now(),
	})
	return err
}

// ExtendLease is the heartbeat a long job uses to stay its owner
// (DESIGN.md 9).
func (q *Queue) ExtendLease(ctx context.Context, job domain.Job, worker string) error {
	return q.db.ExtendLease(ctx, job.ID, worker, q.Now().Add(q.lease))
}

// Retry puts an abandoned job back on the queue.
//
// actor is who asked; a retry always has one, because nothing in this system
// retries on its own -- that is what max_attempts is for.
func (q *Queue) Retry(ctx context.Context, job domain.Job, actor int64) (domain.Job, error) {
	now := q.Now()
	return q.db.RetryJob(ctx, store.RetryRequest{
		JobID: job.ID,
		Now:   now,
		Event: domain.Event{
			Type:    events.JobRetried,
			ActorID: actor,
			Payload: map[string]any{
				"uid":  job.UID,
				"kind": job.Kind,
				// The failure it is being retried out of, recorded because the
				// UPDATE clears the columns that hold it.
				"attempts":   job.Attempts,
				"last_error": job.LastError,
				"failed_at":  job.FailedAt,
			},
			OccurredAt: now,
		},
	})
}

// Job reads one job by uid (invariant 10).
func (q *Queue) Job(ctx context.Context, uid string) (domain.Job, error) {
	return q.db.JobByUID(ctx, uid)
}

// List returns the jobs matching a filter.
func (q *Queue) List(ctx context.Context, f domain.JobFilter) ([]domain.Job, error) {
	return q.db.QueryJobs(ctx, f)
}

// ErrLeaseLost reports that a worker no longer holds the job it was running.
//
// It is what the store's conflict looks like by the time it reaches the worker
// loop, and the loop treats it as a fact rather than a failure: the job has
// been taken by somebody else, that somebody else will report on it, and this
// worker's business with it is over.
var ErrLeaseLost = errors.New("the lease has been lost")

// leaseLost reports whether err is the store refusing a write because the
// lease moved. The store reports it as a conflict, which is what it is.
func leaseLost(err error) bool { return errors.Is(err, domain.ErrConflict) }
