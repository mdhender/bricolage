// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The job queue's vocabulary (DESIGN.md 9, PLAN.md M6).
//
// A job is a piece of work somebody scheduled: what to do, what to do it to,
// when it may start, and how many times it may be tried. Everything here is
// pure -- no clock, no store, no I/O (invariant 1) -- so that lease expiry,
// backoff, and "may this be claimed yet" are decided by functions a test can
// call at any instant it likes.
//
// The one idea worth stating twice is that the lease is a lease. The system we
// learned from set a boolean, so a worker that died mid-job left the row
// marked executing forever and a human had to notice and run an UPDATE. Here
// the claim writes a deadline, every question about the lease is asked against
// an instant, and a worker that never comes back costs one lease duration.

// SubjectJob is the events subject kind for a job. A job's history is a query
// against the events table like everything else's (invariant 7).
const SubjectJob = "job"

// The priority scale (DESIGN.md 9). One is most urgent and five is least,
// which is the order the claim's ORDER BY reads and the order the index is
// built in.
//
// Priority is load-bearing rather than decoration: it is what lets a
// fifty-thousand document republish run at priority 5 without blocking an
// editor pressing Publish. Anything that enqueues in bulk uses PriorityBulk,
// and that is a rule about callers rather than a default this package can
// enforce.
const (
	PriorityUrgent  = 1
	PriorityHigh    = 2
	PriorityNormal  = 3
	PriorityLow     = 4
	PriorityBulk    = 5
	PriorityHighest = PriorityUrgent
	PriorityLowest  = PriorityBulk
)

// DefaultMaxAttempts is how many times a job is tried before it is abandoned.
// It matches the column default, so a row written by hand and a row written by
// this system agree.
const DefaultMaxAttempts = 5

// JobStatus is the answer to "where is this job", derived from the row rather
// than stored beside it.
//
// It is derived on purpose. A status column would be a second statement of
// what completed_at, failed_at, and the lease already say, and the two would
// disagree the first time a lease expired without anybody writing a row --
// which is precisely the case this queue is built to survive.
type JobStatus string

const (
	// JobPending is claimable now, or waiting for its scheduled time.
	JobPending JobStatus = "pending"

	// JobRunning is leased by a worker whose lease has not expired.
	JobRunning JobStatus = "running"

	// JobCompleted finished successfully.
	JobCompleted JobStatus = "completed"

	// JobFailed exhausted its attempts. It is never claimed again.
	JobFailed JobStatus = "failed"
)

// Job is one row of the queue.
//
// Payload is a JSON string rather than a decoded map, for the reason
// Version.Content is: the handler that runs the job knows the shape and
// nothing between here and it does. A publish job's payload names a
// document_version_id and never a document_id (invariant 8), and that is the
// handler's business to state, not this struct's.
type Job struct {
	ID  int64
	UID string

	Kind     string
	Priority int

	// ScheduledFor is the earliest instant the job may be claimed. A job
	// enqueued for now is claimable immediately; one enqueued for later is
	// invisible to Claim until the clock passes it
	// (PLAN.md M6 acceptance 5).
	ScheduledFor time.Time

	Payload string

	// LeaseOwner is the worker holding it, and LeaseExpiresAt is when that
	// stops being true whatever the worker thinks. Both are zero when nobody
	// holds it.
	LeaseOwner     string
	LeaseExpiresAt time.Time

	Attempts    int
	MaxAttempts int

	// LastError is what the most recent failed attempt said. It survives a
	// retry, because "it failed like this four times and then worked" is
	// worth more than a column that is cleared on success.
	LastError string

	CompletedAt time.Time
	FailedAt    time.Time

	// CreatedBy is who scheduled it, or 0 for the system. A job the worker
	// pool enqueues on its own behalf has no user.
	CreatedBy int64

	CreatedAt time.Time
}

// Finished reports whether the job has reached a terminal state.
func (j Job) Finished() bool { return !j.CompletedAt.IsZero() || !j.FailedAt.IsZero() }

// Leased reports whether a worker holds a live lease as of now.
//
// An expired lease is not a lease. That single sentence is the difference
// between a queue that recovers from a dead worker on its own and one that
// needs somebody to notice.
func (j Job) Leased(now time.Time) bool {
	return j.LeaseOwner != "" && now.Before(j.LeaseExpiresAt)
}

// LeaseHeldBy reports whether owner holds a live lease as of now.
//
// Every operation a worker performs on a job it is running asks this, because
// a worker whose lease expired and was claimed by somebody else must not be
// able to complete, fail, or extend the job it no longer holds.
func (j Job) LeaseHeldBy(owner string, now time.Time) bool {
	return owner != "" && j.LeaseOwner == owner && now.Before(j.LeaseExpiresAt)
}

// Claimable reports whether Claim would take this job at now.
//
// It is the Go statement of the SQL predicate in store.ClaimJob, and it exists
// so that a test can say what it expects without writing a second query. The
// two are asserted against each other in internal/store.
func (j Job) Claimable(now time.Time) bool {
	return !j.Finished() && !now.Before(j.ScheduledFor) && !j.Leased(now)
}

// Status derives where the job is as of now.
func (j Job) Status(now time.Time) JobStatus {
	switch {
	case !j.CompletedAt.IsZero():
		return JobCompleted
	case !j.FailedAt.IsZero():
		return JobFailed
	case j.Leased(now):
		return JobRunning
	default:
		return JobPending
	}
}

// Exhausted reports whether an attempt that has just failed was the last one
// allowed.
//
// Attempts is incremented by the claim, so by the time a handler returns an
// error the count already includes the attempt that failed. A job with
// max_attempts 5 whose fifth attempt failed is finished, and asking
// "attempts >= max_attempts" is how that is said without an off-by-one.
func (j Job) Exhausted() bool { return j.Attempts >= j.MaxAttempts }

// NewJob is a job somebody is asking to enqueue.
//
// It is a separate type from Job for the reason NewDocument is: the fields a
// caller supplies are not the fields a row has, and a struct that carried both
// would invite somebody to set completed_at on the way in.
type NewJob struct {
	Kind     string
	Priority int

	// ScheduledFor is when the job becomes claimable. The zero time means
	// "now", which the caller resolves from its clock before calling Validate:
	// this package does not read one (invariant 3).
	ScheduledFor time.Time

	// Payload is JSON, and empty means "{}".
	Payload string

	MaxAttempts int

	CreatedBy int64
}

// Normalize fills in the defaults. It is pure; the caller writes the result.
func (n NewJob) Normalize(now time.Time) NewJob {
	n.Kind = strings.TrimSpace(n.Kind)
	if n.Priority == 0 {
		n.Priority = PriorityNormal
	}
	if n.MaxAttempts == 0 {
		n.MaxAttempts = DefaultMaxAttempts
	}
	if n.ScheduledFor.IsZero() {
		n.ScheduledFor = now
	}
	n.ScheduledFor = n.ScheduledFor.UTC()
	n.Payload = NormalizePayload(n.Payload)
	return n
}

// Validate reports whether the job is one the store will accept.
func (n NewJob) Validate() error {
	if strings.TrimSpace(n.Kind) == "" {
		return fmt.Errorf("job: no kind: %w", ErrInvalid)
	}
	if n.Priority < PriorityHighest || n.Priority > PriorityLowest {
		return fmt.Errorf("job %s: priority %d: want %d to %d: %w",
			n.Kind, n.Priority, PriorityHighest, PriorityLowest, ErrInvalid)
	}
	if n.MaxAttempts < 1 {
		return fmt.Errorf("job %s: max_attempts %d: a job nobody may attempt is not a job: %w",
			n.Kind, n.MaxAttempts, ErrInvalid)
	}
	return ValidatePayload(n.Payload)
}

// ValidatePayload reports whether s is storable in jobs.payload: a JSON
// object, or empty, which Normalize renders as "{}".
//
// A payload is what a handler is given, and a handler parses it. What this
// refuses is a payload no handler could parse at all -- which is worth
// refusing at enqueue time, because the alternative is discovering it once per
// attempt, five attempts later, in a worker log.
func ValidatePayload(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var into map[string]any
	if err := json.Unmarshal([]byte(s), &into); err != nil {
		return fmt.Errorf("payload: not a JSON object: %v: %w", err, ErrInvalid)
	}
	return nil
}

// NormalizePayload renders a payload for storage. The empty string becomes an
// empty object rather than a NOT NULL column holding "".
func NormalizePayload(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// RetryDelay is how long to wait before a failed attempt may be tried again.
//
// It doubles per attempt from RetryBaseDelay and stops at RetryMaxDelay. The
// shape matters more than the constants: a job that fails because something it
// depends on is down must not spend its five attempts in the first second,
// which is exactly what a fixed delay of zero does. Retrying immediately is
// how a queue turns one outage into five failures and a job somebody has to
// find by hand.
//
// It is pure and it takes the attempt count rather than a clock, so the
// schedule a job gets is a value a test can assert on.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := RetryBaseDelay
	for range attempt - 1 {
		d *= 2
		if d >= RetryMaxDelay {
			return RetryMaxDelay
		}
	}
	return d
}

// The retry backoff bounds. The first retry waits ten seconds and the fifth
// would wait past the cap, so a job with the default five attempts is given up
// on within a few minutes rather than a few days.
const (
	RetryBaseDelay = 10 * time.Second
	RetryMaxDelay  = 5 * time.Minute
)

// JobFilter is the question "earl job list" and GET /api/v1/jobs ask.
//
// Pending and Failed are separate booleans rather than one status, because the
// two are different questions with different answers -- "what is waiting" and
// "what did I lose" -- and because a status field would need a fifth value for
// "any" that read as a state a job could be in.
type JobFilter struct {
	// Kind constrains the list to one job kind, or "" for any.
	Kind string

	// Pending lists the jobs that have neither completed nor failed, in the
	// order they will run.
	Pending bool

	// Failed lists the jobs that exhausted their attempts, newest failure
	// first.
	Failed bool

	// Limit is how many rows to return; zero means DefaultJobListLimit.
	Limit int
}

// The bounds on a job listing. They are the document list's bounds and for the
// same reason: a queue is something a person reads.
const (
	DefaultJobListLimit = 100
	MaxJobListLimit     = 1000
)

// Validate reports whether the filter is one the system will accept.
func (f JobFilter) Validate() error {
	if f.Pending && f.Failed {
		return fmt.Errorf("a job is pending or failed, not both: %w", ErrInvalid)
	}
	if f.Limit < 0 {
		return fmt.Errorf("limit %d: not a count: %w", f.Limit, ErrInvalid)
	}
	if f.Limit > MaxJobListLimit {
		return fmt.Errorf("limit %d: at most %d: %w", f.Limit, MaxJobListLimit, ErrInvalid)
	}
	return nil
}

// Normalize returns the filter with its defaults filled in.
func (f JobFilter) Normalize() JobFilter {
	f.Kind = strings.TrimSpace(f.Kind)
	if f.Limit <= 0 {
		f.Limit = DefaultJobListLimit
	}
	return f
}

// Describe renders the filter as the sentence a CLI prints above an empty
// list, so that "no jobs" says which question was asked.
func (f JobFilter) Describe() string {
	var parts []string
	if f.Pending {
		parts = append(parts, "pending")
	}
	if f.Failed {
		parts = append(parts, "failed")
	}
	if f.Kind != "" {
		parts = append(parts, "of kind "+f.Kind)
	}
	if len(parts) == 0 {
		return "jobs"
	}
	return "jobs " + strings.Join(parts, ", ")
}
