// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// The job queue's SQL (DESIGN.md 9, PLAN.md M6). All of it is here
// (invariant 2); internal/jobs decides what runs and this decides nothing.
//
// Three things in this file carry more weight than they look like.
//
// The claim is one statement. It is a compare-and-swap: the UPDATE selects its
// own victim in a subquery and RETURNS what it took, so between choosing a job
// and marking it taken there is no window at all -- not a narrow one, none.
// Twenty workers running it against one ready job produce one winner and
// nineteen empty results, and that is a property of the statement rather than
// of the mutex this package happens to serialize writers with
// (PLAN.md M6 acceptance 1).
//
// Every operation a worker performs on a running job names its lease. A worker
// whose lease expired while it was working has already been overtaken: another
// worker may hold the job, and letting the first one complete it would record
// a success for work the second is still doing. So Complete, Fail, Extend, and
// Release all match on lease_owner, and a mismatch is a conflict rather than a
// silent no-op.
//
// "now" is a parameter, never datetime('now'). Lease expiry and scheduling are
// evaluated against the injected clock (invariant 3), and a SQL function
// reading the wall clock would be a second, untestable source of the time
// inside the statements that most need a fake one.

// jobColumns is the projection every job read shares.
const jobColumns = `id, uid, kind, priority, scheduled_for, payload,
	lease_owner, lease_expires_at, attempts, max_attempts, last_error,
	completed_at, failed_at, created_by, created_at`

// NewJob is a job to enqueue, with its uid already minted by the caller: uids
// are ULIDs over an instant and this package has no clock (invariant 3).
type NewJob struct {
	UID string
	Job domain.NewJob

	// Event is recorded in the same transaction as the insert (invariant 7).
	// Its subject is filled in here, because only this function knows the id.
	Event domain.Event
}

// EnqueueJob writes a job and its event in one transaction.
func (db *DB) EnqueueJob(ctx context.Context, n NewJob) (domain.Job, error) {
	if err := n.Job.Validate(); err != nil {
		return domain.Job{}, err
	}
	if n.UID == "" {
		return domain.Job{}, fmt.Errorf("job %s: no uid: %w", n.Job.Kind, domain.ErrInvalid)
	}

	var out domain.Job
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		var err error
		out, err = enqueueJob(conn, n)
		return err
	})
	return out, err
}

// enqueueJob writes a job and its event on a connection the caller holds,
// inside the caller's transaction.
//
// It exists because enqueuing is not always its own transaction. DESIGN.md 6.4
// makes "enqueue any jobs" the fourth step of a transition, after the state,
// the effects, and the event, and all four are one change: a transition into
// "published" whose publish job was written by a second transaction could
// commit the move and lose the publish.
func enqueueJob(conn *sqlite.Conn, n NewJob) (domain.Job, error) {
	if err := n.Job.Validate(); err != nil {
		return domain.Job{}, err
	}
	if n.UID == "" {
		return domain.Job{}, fmt.Errorf("job %s: no uid: %w", n.Job.Kind, domain.ErrInvalid)
	}

	err := run(conn, "enqueuing a "+n.Job.Kind+" job", `
		INSERT INTO jobs (uid, kind, priority, scheduled_for, payload,
		                  attempts, max_attempts, created_by, created_at)
		VALUES (:uid, :kind, :priority, :scheduled_for, :payload,
		        0, :max_attempts, :created_by, :created_at)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":uid", n.UID)
			stmt.SetText(":kind", n.Job.Kind)
			stmt.SetInt64(":priority", int64(n.Job.Priority))
			stmt.SetText(":scheduled_for", formatTime(n.Job.ScheduledFor))
			stmt.SetText(":payload", n.Job.Payload)
			stmt.SetInt64(":max_attempts", int64(n.Job.MaxAttempts))
			if n.Job.CreatedBy == 0 {
				// NULL is the system, which is why the column is nullable
				// rather than pointing at a sentinel row.
				stmt.SetNull(":created_by")
			} else {
				stmt.SetInt64(":created_by", n.Job.CreatedBy)
			}
			stmt.SetText(":created_at", formatTime(n.Event.OccurredAt))
		}, nil)
	if err != nil {
		return domain.Job{}, err
	}
	id := conn.LastInsertRowID()

	n.Event.SubjectKind = domain.SubjectJob
	n.Event.SubjectID = id
	if _, err := recordEvent(conn, n.Event); err != nil {
		return domain.Job{}, err
	}
	var out domain.Job
	return out, jobByID(conn, id, &out)
}

// claimSQL is the compare-and-swap, DESIGN.md 9's statement with its ORDER BY
// and this schema's index in agreement.
//
// It is a package-level constant so that the test which runs EXPLAIN QUERY
// PLAN over it is explaining the statement ClaimJob runs rather than a
// hand-written approximation of it -- asserting on an approximation would
// prove nothing about the query.
//
// The subquery's ORDER BY is priority, then schedule, then id, which is
// exactly the key of jobs_claimable, so the plan is a search with no temp
// b-tree and LIMIT 1 stops it at the first row that also satisfies the two
// time filters.
const claimSQL = `
	UPDATE jobs
	   SET lease_owner = :owner,
	       lease_expires_at = :deadline,
	       attempts = attempts + 1
	 WHERE id = (
	       SELECT id FROM jobs
	        WHERE completed_at IS NULL AND failed_at IS NULL
	          AND scheduled_for <= :now
	          AND (lease_expires_at IS NULL OR lease_expires_at < :now)
	        ORDER BY priority ASC, scheduled_for ASC, id ASC
	        LIMIT 1)
	RETURNING ` + jobColumns

// ClaimRequest is one worker asking for work.
type ClaimRequest struct {
	// Owner names the worker. It is text and not a user: a worker is not a
	// row in "users" and never will be.
	Owner string

	// Now is the instant the schedule and the lease are evaluated against.
	Now time.Time

	// Lease is how long the claim is good for. A worker that is still running
	// when it expires has lost the job (ExtendLease is how it does not).
	Lease time.Duration
}

// ClaimJob takes at most one job for the worker, atomically.
//
// It returns ok false when nothing is ready, which is the ordinary case for an
// idle queue and not an error. The claim increments attempts in the same
// statement, so a worker that dies between claiming and recording anything has
// still spent an attempt -- which is what stops a job that kills its worker
// from being retried forever (PLAN.md M6 acceptance 2).
//
// The rows this returns are ordered by priority and then by schedule
// (PLAN.md M6 acceptance 4), because the subquery says so and the index
// agrees.
func (db *DB) ClaimJob(ctx context.Context, req ClaimRequest) (domain.Job, bool, error) {
	if strings.TrimSpace(req.Owner) == "" {
		return domain.Job{}, false, fmt.Errorf("claiming a job: no worker name: %w", domain.ErrInvalid)
	}
	if req.Lease <= 0 {
		return domain.Job{}, false, fmt.Errorf("claiming a job: lease %s: not a duration: %w",
			req.Lease, domain.ErrInvalid)
	}

	var (
		out     domain.Job
		found   bool
		scanErr error
	)
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "claiming a job", claimSQL,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":owner", req.Owner)
				stmt.SetText(":deadline", formatTime(req.Now.Add(req.Lease)))
				stmt.SetText(":now", formatTime(req.Now))
			},
			func(stmt *sqlite.Stmt) error {
				found = true
				out, scanErr = scanJob(stmt)
				return scanErr
			})
	})
	if err != nil {
		return domain.Job{}, false, err
	}
	return out, found, nil
}

// JobOutcome is what a worker reports about an attempt it ran.
type JobOutcome struct {
	JobID int64

	// Owner is the lease the worker holds. Every write below matches on it:
	// a worker whose lease expired has been overtaken, and letting it record
	// an outcome would record it over somebody else's work.
	Owner string

	Now time.Time

	// Event is recorded in the same transaction as the outcome
	// (invariant 7). Its subject is filled in here.
	Event domain.Event
}

// CompleteJob marks a job done and releases its lease.
//
// It clears the lease as well as setting completed_at, so that a finished job
// carries no owner: a row that says both "completed" and "held by worker-3"
// is a row that has to be read twice to be understood.
func (db *DB) CompleteJob(ctx context.Context, out JobOutcome) (domain.Job, error) {
	return db.finish(ctx, out, `
		UPDATE jobs
		   SET completed_at = :now,
		       lease_owner = NULL,
		       lease_expires_at = NULL
		 WHERE id = :id AND lease_owner = :owner`, nil)
}

// FailJob records a failed attempt.
//
// One statement writes both outcomes, because "retry it later" and "give up"
// are the same decision made from the same row and two statements would need
// the count read twice. retryAt is when the next attempt may start; when the
// attempts are exhausted, failed_at is set instead and the job is never
// claimed again (PLAN.md M6 acceptance 3).
//
// The lease is released either way. A failed attempt is over, and a job whose
// retry is scheduled for ten seconds' time must be claimable then rather than
// when a lease nobody holds happens to expire.
func (db *DB) FailJob(ctx context.Context, out JobOutcome, message string, retryAt time.Time) (domain.Job, error) {
	return db.finish(ctx, out, `
		UPDATE jobs
		   SET last_error = :error,
		       lease_owner = NULL,
		       lease_expires_at = NULL,
		       scheduled_for = CASE WHEN attempts >= max_attempts THEN scheduled_for ELSE :retry_at END,
		       failed_at     = CASE WHEN attempts >= max_attempts THEN :now         ELSE NULL      END
		 WHERE id = :id AND lease_owner = :owner`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":error", message)
			stmt.SetText(":retry_at", formatTime(retryAt))
		})
}

// ReleaseJob gives a job back without recording an attempt's outcome.
//
// It is what graceful shutdown does with work a worker was still holding
// (PLAN.md M6 acceptance 6): the process is going away, the job is not
// finished and has not failed, and the honest thing is to drop the lease so
// that the next worker to look finds it immediately rather than in a lease's
// time.
//
// attempts is deliberately not decremented. The attempt happened; a queue that
// forgot attempts it had made would let a job that reliably kills its worker
// run forever.
func (db *DB) ReleaseJob(ctx context.Context, out JobOutcome) (domain.Job, error) {
	return db.finish(ctx, out, `
		UPDATE jobs
		   SET lease_owner = NULL,
		       lease_expires_at = NULL,
		       scheduled_for = :now
		 WHERE id = :id AND lease_owner = :owner`, nil)
}

// finish is the shape Complete, Fail, and Release share: one UPDATE matched on
// the lease, one event, one transaction, and the row read back.
//
// The lease match is the point of the shared shape. A worker whose lease
// expired while it worked has been overtaken, and every one of these three
// must refuse rather than overwrite; writing the match three times is how one
// of them ends up without it.
func (db *DB) finish(ctx context.Context, out JobOutcome, query string, extra func(*sqlite.Stmt)) (domain.Job, error) {
	if strings.TrimSpace(out.Owner) == "" {
		return domain.Job{}, fmt.Errorf("job %d: no worker name: %w", out.JobID, domain.ErrInvalid)
	}
	var job domain.Job
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("finishing job %d", out.JobID), query,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":id", out.JobID)
				stmt.SetText(":owner", out.Owner)
				stmt.SetText(":now", formatTime(out.Now))
				if extra != nil {
					extra(stmt)
				}
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() != 1 {
			// Either the job is gone or the lease is somebody else's. Both are
			// conflicts rather than "not found": the worker had it a moment
			// ago, and what changed is the world rather than the request.
			return fmt.Errorf("job %d is no longer leased by %q: %w",
				out.JobID, out.Owner, domain.ErrConflict)
		}

		if out.Event.Type != "" {
			out.Event.SubjectKind = domain.SubjectJob
			out.Event.SubjectID = out.JobID
			if _, err := recordEvent(conn, out.Event); err != nil {
				return err
			}
		}
		return jobByID(conn, out.JobID, &job)
	})
	return job, err
}

// ExtendLease pushes a worker's lease out, which is how a long job stays its
// owner (DESIGN.md 9, "Long jobs must heartbeat by extending their lease").
//
// It writes no event: a heartbeat records nothing about the world that is
// still true one lease later, and a job that beats every thirty seconds for an
// hour would write a hundred and twenty rows saying it was still running.
func (db *DB) ExtendLease(ctx context.Context, jobID int64, owner string, until time.Time) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("job %d: no worker name: %w", jobID, domain.ErrInvalid)
	}
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("extending the lease on job %d", jobID), `
			UPDATE jobs
			   SET lease_expires_at = :until
			 WHERE id = :id AND lease_owner = :owner`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":until", formatTime(until))
				stmt.SetInt64(":id", jobID)
				stmt.SetText(":owner", owner)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() != 1 {
			return fmt.Errorf("job %d is no longer leased by %q: %w", jobID, owner, domain.ErrConflict)
		}
		return nil
	})
}

// RetryRequest is somebody putting an abandoned job back on the queue.
type RetryRequest struct {
	JobID int64

	// Now is when the retried job becomes claimable. A retry runs now: the
	// person asking for it is watching.
	Now time.Time

	// Event is recorded in the same transaction (invariant 7).
	Event domain.Event
}

// RetryJob clears the failure and makes an abandoned job claimable again.
//
// It resets attempts to zero, because a retry is a decision to try the whole
// thing again rather than to squeeze one more attempt out of an exhausted
// budget -- a job put back with attempts at its maximum would fail once and be
// abandoned again, which is not what the person pressing the button meant.
//
// last_error survives. "It failed like this five times and then worked" is
// worth more than a column cleared on the way past, and the JobRetried event
// carries the failure it was retried out of in any case.
func (db *DB) RetryJob(ctx context.Context, req RetryRequest) (domain.Job, error) {
	var out domain.Job
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		if err := jobByID(conn, req.JobID, &out); err != nil {
			return err
		}
		if out.FailedAt.IsZero() {
			return fmt.Errorf("job %s has not failed; there is nothing to retry: %w",
				out.UID, domain.ErrConflict)
		}

		err := run(conn, fmt.Sprintf("retrying job %d", req.JobID), `
			UPDATE jobs
			   SET failed_at = NULL,
			       attempts = 0,
			       lease_owner = NULL,
			       lease_expires_at = NULL,
			       scheduled_for = :now
			 WHERE id = :id AND failed_at IS NOT NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(req.Now))
				stmt.SetInt64(":id", req.JobID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() != 1 {
			return fmt.Errorf("job %s was retried by somebody else first: %w", out.UID, domain.ErrConflict)
		}

		req.Event.SubjectKind = domain.SubjectJob
		req.Event.SubjectID = req.JobID
		if _, err := recordEvent(conn, req.Event); err != nil {
			return err
		}
		return jobByID(conn, req.JobID, &out)
	})
	return out, err
}

// JobByUID reads one job by its external identifier (invariant 10).
func (db *DB) JobByUID(ctx context.Context, uid string) (domain.Job, error) {
	var out domain.Job
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("job %q", uid),
			`SELECT `+jobColumns+` FROM jobs WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanJob(stmt)
				return err
			})
	})
	return out, err
}

// jobByID reads one job on a connection the caller holds. The id always comes
// from a row this process already read (invariant 10).
func jobByID(conn *sqlite.Conn, id int64, j *domain.Job) error {
	return one(conn, fmt.Sprintf("job %d", id),
		`SELECT `+jobColumns+` FROM jobs WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*j, err = scanJob(stmt)
			return err
		})
}

// jobQuerySQL builds the listing statement and its binder.
//
// It is one function returning both halves for the reason documentQuerySQL is:
// a clause can never be added to the text without its parameter, and the two
// drifting apart is how a query silently matches everything.
//
// The order depends on the question, and deliberately so. "Pending" is asked
// by somebody who wants to know what will run next, so it comes back in the
// order Claim will take it -- the key of jobs_claimable, so no sort. "Failed"
// is asked by somebody looking for what broke, so it comes back newest failure
// first, off jobs_failed. Everything else is newest first, off the primary
// key. A single ORDER BY for all three would be one that answered none of them
// and sorted the table to do it.
func jobQuerySQL(f domain.JobFilter) (string, func(*sqlite.Stmt)) {
	f = f.Normalize()

	var (
		where []string
		binds []func(*sqlite.Stmt)
	)
	switch {
	case f.Pending:
		// Stated as the index's own condition so that jobs_claimable is
		// usable: SQLite may only use a partial index when the query's WHERE
		// implies the index's.
		where = append(where, "completed_at IS NULL AND failed_at IS NULL")
	case f.Failed:
		where = append(where, "failed_at IS NOT NULL")
	}
	if f.Kind != "" {
		where = append(where, "kind = :kind")
		binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetText(":kind", f.Kind) })
	}

	query := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		query += "\n\t\t WHERE " + strings.Join(where, "\n\t\t   AND ")
	}
	switch {
	case f.Pending:
		query += "\n\t\t ORDER BY priority ASC, scheduled_for ASC, id ASC"
	case f.Failed:
		query += "\n\t\t ORDER BY failed_at DESC"
	default:
		query += "\n\t\t ORDER BY id DESC"
	}
	query += "\n\t\t LIMIT :limit"
	binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetInt64(":limit", int64(f.Limit)) })

	return query, func(stmt *sqlite.Stmt) {
		for _, bind := range binds {
			bind(stmt)
		}
	}
}

// QueryJobs returns the jobs matching a filter.
func (db *DB) QueryJobs(ctx context.Context, f domain.JobFilter) ([]domain.Job, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	query, bind := jobQuerySQL(f)

	var out []domain.Job
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing "+f.Describe(), query, bind, func(stmt *sqlite.Stmt) error {
			j, err := scanJob(stmt)
			if err != nil {
				return err
			}
			out = append(out, j)
			return nil
		})
	})
	return out, err
}

// stuckLeases counts the jobs holding a lease that expired before now
// (PLAN.md M6, "cmsdb check now reports leases held past expiry").
//
// A count above zero is not damage. It is what a worker that died looks like
// from the outside, and the queue recovers from it on its own: the next claim
// takes the job because an expired lease is not a lease. What it means to an
// operator is "something killed a worker and did not restart it", which is
// worth a line in a check that runs in a cron job.
//
// It reads jobs_leased, which is partial on exactly the rows a lease is held
// over, so it touches only the jobs somebody is running right now. It takes a
// connection because its one caller, Check, already holds a reader and asks
// several questions of the same one.
func stuckLeases(conn *sqlite.Conn, now time.Time) (int, error) {
	var n int
	err := one(conn, "counting stuck job leases", `
		SELECT count(*) AS n
		  FROM jobs
		 WHERE lease_expires_at IS NOT NULL
		   AND completed_at IS NULL AND failed_at IS NULL
		   AND lease_expires_at < :now`,
		func(stmt *sqlite.Stmt) { stmt.SetText(":now", formatTime(now)) },
		func(stmt *sqlite.Stmt) error {
			n = int(stmt.GetInt64("n"))
			return nil
		})
	return n, err
}

func scanJob(stmt *sqlite.Stmt) (domain.Job, error) {
	id := stmt.GetInt64("id")

	scheduled, err := parseTime(stmt.GetText("scheduled_for"))
	if err != nil {
		return domain.Job{}, fmt.Errorf("job %d: scheduled_for: %w", id, err)
	}
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Job{}, fmt.Errorf("job %d: created_at: %w", id, err)
	}

	j := domain.Job{
		ID:           id,
		UID:          stmt.GetText("uid"),
		Kind:         stmt.GetText("kind"),
		Priority:     int(stmt.GetInt64("priority")),
		ScheduledFor: scheduled,
		Payload:      stmt.GetText("payload"),
		Attempts:     int(stmt.GetInt64("attempts")),
		MaxAttempts:  int(stmt.GetInt64("max_attempts")),
		CreatedAt:    created,
	}
	if s := nullText(stmt, "lease_owner"); s != nil {
		j.LeaseOwner = *s
	}
	if s := nullText(stmt, "last_error"); s != nil {
		j.LastError = *s
	}
	if v := nullInt64(stmt, "created_by"); v != nil {
		j.CreatedBy = *v
	}
	for _, f := range []struct {
		column string
		into   *time.Time
	}{
		{"lease_expires_at", &j.LeaseExpiresAt},
		{"completed_at", &j.CompletedAt},
		{"failed_at", &j.FailedAt},
	} {
		s := nullText(stmt, f.column)
		if s == nil {
			continue
		}
		t, err := parseTime(*s)
		if err != nil {
			return domain.Job{}, fmt.Errorf("job %d: %s: %w", id, f.column, err)
		}
		*f.into = t
	}
	return j, nil
}
