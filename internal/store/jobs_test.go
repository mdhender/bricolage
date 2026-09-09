// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"zombiezen.com/go/sqlite"
)

// The queue's store tests (PLAN.md M6). Five of the six acceptance criteria
// are about what the SQL does under concurrency and against a clock, so they
// live here, against a real in-memory database with every migration applied
// through the same code path cmsdb uses.

// jobFixture is a store with a clock a test drives by hand.
type jobFixture struct {
	db  *DB
	now time.Time
}

func newJobFixture(t *testing.T) *jobFixture {
	t.Helper()
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &jobFixture{db: db, now: fixedClock().Now()}
}

// enqueue writes a job, defaulting everything the test did not say.
func (f *jobFixture) enqueue(t *testing.T, n domain.NewJob) domain.Job {
	t.Helper()
	n = n.Normalize(f.now)
	job, err := f.db.EnqueueJob(t.Context(), NewJob{
		UID: ids.MustNew(f.now),
		Job: n,
		Event: domain.Event{
			Type:       events.JobEnqueued,
			Payload:    map[string]any{"kind": n.Kind},
			OccurredAt: f.now,
		},
	})
	if err != nil {
		t.Fatalf("EnqueueJob(%+v): %v", n, err)
	}
	return job
}

// claim takes a job for worker at the fixture's current instant.
func (f *jobFixture) claim(t *testing.T, worker string, lease time.Duration) (domain.Job, bool) {
	t.Helper()
	job, ok, err := f.db.ClaimJob(t.Context(), ClaimRequest{Owner: worker, Now: f.now, Lease: lease})
	if err != nil {
		t.Fatalf("ClaimJob(%s): %v", worker, err)
	}
	return job, ok
}

// TestEnqueueWritesTheJobAndItsEvent is invariant 7 for the queue: the row and
// the event commit together, and the defaults the column declares and the ones
// domain.NewJob declares are the same defaults.
func TestEnqueueWritesTheJobAndItsEvent(t *testing.T) {
	f := newJobFixture(t)

	job := f.enqueue(t, domain.NewJob{Kind: "noop"})
	if job.UID == "" || job.ID == 0 {
		t.Fatalf("the job came back without an identity: %+v", job)
	}
	if job.Priority != domain.PriorityNormal {
		t.Errorf("priority = %d, want the default %d", job.Priority, domain.PriorityNormal)
	}
	if job.MaxAttempts != domain.DefaultMaxAttempts {
		t.Errorf("max_attempts = %d, want the default %d", job.MaxAttempts, domain.DefaultMaxAttempts)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d on a job nobody has claimed", job.Attempts)
	}
	if job.Payload != "{}" {
		t.Errorf("payload = %q, want the empty object the column defaults to", job.Payload)
	}
	if got := job.Status(f.now); got != domain.JobPending {
		t.Errorf("status = %q, want %q", got, domain.JobPending)
	}

	history, err := f.db.EventsForSubject(t.Context(), domain.SubjectJob, job.ID, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(history) != 1 || history[0].Type != events.JobEnqueued {
		t.Errorf("history = %v, want one %s", history, events.JobEnqueued)
	}
}

// TestClaimIsExactlyOnce is PLAN.md M6 acceptance 1: twenty goroutines call
// Claim against one ready job and exactly one gets it.
//
// It is a test about the statement, not about the mutex this package happens
// to serialize writers with. The UPDATE picks its own victim in a subquery and
// RETURNS what it took, so there is no window between choosing and taking --
// which is what would have to be true if the writers were not serialized, and
// will have to stay true when a second process runs workers against the same
// file.
//
// Run under -race, which is where it earns its keep (AGENTS.md, "Testing").
func TestClaimIsExactlyOnce(t *testing.T) {
	f := newJobFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: "noop"})

	const racers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})
	for i := range racers {
		worker := "worker-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		wg.Go(func() {
			<-start
			got, ok, err := f.db.ClaimJob(t.Context(), ClaimRequest{
				Owner: worker, Now: f.now, Lease: time.Minute,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("ClaimJob(%s): %v", worker, err)
				return
			}
			if ok {
				if got.ID != job.ID {
					t.Errorf("%s claimed job %d, and there is only job %d", worker, got.ID, job.ID)
				}
				winners = append(winners, worker)
			}
		})
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d of %d claims succeeded against one ready job: %v", len(winners), racers, winners)
	}

	// The winner holds the lease and has spent exactly one attempt: nineteen
	// losing claims must not have incremented anything.
	after, err := f.db.JobByUID(t.Context(), job.UID)
	if err != nil {
		t.Fatalf("JobByUID: %v", err)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d after one successful claim among %d", after.Attempts, racers)
	}
	if !after.LeaseHeldBy(winners[0], f.now) {
		t.Errorf("the lease is %q, want %q", after.LeaseOwner, winners[0])
	}
}

// TestAnExpiredLeaseIsReclaimable is PLAN.md M6 acceptance 2: a claimed job
// whose lease expires is claimable again, and attempts is 2.
//
// This is the defect the system we learned from could not recover from. It
// marked a row "executing" with no lease and no heartbeat, so a worker that
// died left the row marked executing forever and somebody had to notice and
// run an UPDATE. Here the recovery is the passage of time.
func TestAnExpiredLeaseIsReclaimable(t *testing.T) {
	f := newJobFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: "noop"})

	const lease = time.Minute
	first, ok := f.claim(t, "worker-1", lease)
	if !ok {
		t.Fatal("the first claim found nothing to do")
	}
	if first.Attempts != 1 {
		t.Errorf("attempts = %d after the first claim, want 1", first.Attempts)
	}

	// While the lease is live nobody else may have it, however dead worker-1
	// is.
	f.now = f.now.Add(lease - time.Second)
	if _, ok := f.claim(t, "worker-2", lease); ok {
		t.Fatal("a second worker claimed a job whose lease had not expired")
	}

	// One second past the deadline it is anybody's.
	f.now = f.now.Add(2 * time.Second)
	second, ok := f.claim(t, "worker-2", lease)
	if !ok {
		t.Fatal("the job was not reclaimable after its lease expired")
	}
	if second.ID != job.ID {
		t.Fatalf("worker-2 claimed job %d, want %d", second.ID, job.ID)
	}
	if second.Attempts != 2 {
		t.Errorf("attempts = %d after the lease expired and the job was reclaimed, want 2", second.Attempts)
	}
	if second.LeaseOwner != "worker-2" {
		t.Errorf("lease_owner = %q, want worker-2", second.LeaseOwner)
	}
}

// TestExhaustedAttemptsAbandonTheJob is PLAN.md M6 acceptance 3: a job failing
// max_attempts times sets failed_at and last_error and is not claimed again.
func TestExhaustedAttemptsAbandonTheJob(t *testing.T) {
	f := newJobFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: "noop", MaxAttempts: 3})

	for attempt := 1; attempt <= job.MaxAttempts; attempt++ {
		claimed, ok := f.claim(t, "worker-1", time.Minute)
		if !ok {
			t.Fatalf("attempt %d found nothing to claim", attempt)
		}
		if claimed.Attempts != attempt {
			t.Fatalf("attempts = %d on attempt %d", claimed.Attempts, attempt)
		}

		message := "attempt " + string(rune('0'+attempt)) + " went wrong"
		retryAt := f.now.Add(domain.RetryDelay(claimed.Attempts))
		after, err := f.db.FailJob(t.Context(), JobOutcome{
			JobID: claimed.ID, Owner: "worker-1", Now: f.now,
			Event: domain.Event{Type: events.JobFailed, OccurredAt: f.now},
		}, message, retryAt)
		if err != nil {
			t.Fatalf("FailJob on attempt %d: %v", attempt, err)
		}
		if after.LastError != message {
			t.Errorf("last_error = %q, want %q", after.LastError, message)
		}

		if attempt < job.MaxAttempts {
			if !after.FailedAt.IsZero() {
				t.Fatalf("attempt %d of %d set failed_at", attempt, job.MaxAttempts)
			}
			// The retry is scheduled with backoff, so the next claim needs the
			// clock moved past it. That the job is invisible until then is
			// acceptance 5 again, from the retry side.
			if _, ok := f.claim(t, "worker-2", time.Minute); ok {
				t.Fatalf("attempt %d was retried before its backoff elapsed", attempt)
			}
			f.now = retryAt
			continue
		}

		if after.FailedAt.IsZero() {
			t.Fatalf("the last of %d attempts did not set failed_at: %+v", job.MaxAttempts, after)
		}
		if got := after.Status(f.now); got != domain.JobFailed {
			t.Errorf("status = %q after the last attempt, want %q", got, domain.JobFailed)
		}
	}

	// Not claimed again, ever: an abandoned job waits for a person.
	f.now = f.now.Add(365 * 24 * time.Hour)
	if _, ok := f.claim(t, "worker-3", time.Minute); ok {
		t.Error("a job that exhausted its attempts was claimed a year later")
	}

	// And the retry is what puts it back.
	failed, err := f.db.JobByUID(t.Context(), job.UID)
	if err != nil {
		t.Fatalf("JobByUID: %v", err)
	}
	retried, err := f.db.RetryJob(t.Context(), RetryRequest{
		JobID: failed.ID, Now: f.now,
		Event: domain.Event{Type: events.JobRetried, OccurredAt: f.now},
	})
	if err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	if !retried.FailedAt.IsZero() || retried.Attempts != 0 {
		t.Errorf("a retried job is still failed: %+v", retried)
	}
	if retried.LastError == "" {
		t.Error("the retry cleared last_error; how it failed is worth keeping")
	}
	if _, ok := f.claim(t, "worker-3", time.Minute); !ok {
		t.Error("a retried job was not claimable")
	}

	// Retrying something that has not failed is refused rather than quietly
	// resetting a job somebody is running.
	if _, err := f.db.RetryJob(t.Context(), RetryRequest{
		JobID: failed.ID, Now: f.now,
		Event: domain.Event{Type: events.JobRetried, OccurredAt: f.now},
	}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("retrying a job that has not failed returned %v, want a conflict", err)
	}
}

// TestClaimOrder is PLAN.md M6 acceptance 4: Claim returns jobs in priority
// then schedule order, tested with a mixed set.
//
// Priority is load-bearing rather than decoration (DESIGN.md 9). This is the
// test that says so: the bulk job enqueued first, and due first, still waits
// for the urgent one enqueued last.
func TestClaimOrder(t *testing.T) {
	f := newJobFixture(t)
	base := f.now

	// Deliberately mixed: the insertion order, the priority order, and the
	// schedule order are three different orders, so a claim that honoured the
	// wrong one cannot pass by accident.
	type spec struct {
		name     string
		priority int
		offset   time.Duration
	}
	specs := []spec{
		{"bulk-oldest", domain.PriorityBulk, -3 * time.Hour},
		{"normal-old", domain.PriorityNormal, -2 * time.Hour},
		{"urgent-newest", domain.PriorityUrgent, -1 * time.Minute},
		{"normal-older", domain.PriorityNormal, -150 * time.Minute},
		{"urgent-older", domain.PriorityUrgent, -30 * time.Minute},
		{"bulk-newest", domain.PriorityBulk, -1 * time.Second},
	}
	uids := make(map[string]string, len(specs))
	for _, s := range specs {
		job := f.enqueue(t, domain.NewJob{
			Kind:         "noop",
			Priority:     s.priority,
			ScheduledFor: base.Add(s.offset),
		})
		uids[job.UID] = s.name
	}

	want := []string{
		"urgent-older", "urgent-newest", // priority 1, oldest schedule first
		"normal-older", "normal-old", // priority 3
		"bulk-oldest", "bulk-newest", // priority 5
	}
	var got []string
	for range specs {
		// A fresh lease each time, so the claimed job leaves the pool without
		// the test having to complete it.
		job, ok := f.claim(t, "worker-1", time.Hour)
		if !ok {
			break
		}
		got = append(got, uids[job.UID])
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("claim order:\n got %v\nwant %v", got, want)
	}
}

// TestAFutureJobIsNotClaimedUntilItsTime is PLAN.md M6 acceptance 5: a job
// scheduled for the future is not claimed until the fake clock passes it.
func TestAFutureJobIsNotClaimedUntilItsTime(t *testing.T) {
	f := newJobFixture(t)
	when := f.now.Add(time.Hour)
	job := f.enqueue(t, domain.NewJob{Kind: "noop", ScheduledFor: when})

	for _, at := range []time.Time{f.now, when.Add(-time.Second)} {
		f.now = at
		if _, ok := f.claim(t, "worker-1", time.Minute); ok {
			t.Fatalf("a job scheduled for %s was claimed at %s", when, at)
		}
	}

	// At the instant it is due, and not one later: "scheduled_for <= now".
	f.now = when
	claimed, ok := f.claim(t, "worker-1", time.Minute)
	if !ok {
		t.Fatalf("the job was not claimable at %s, the instant it was scheduled for", when)
	}
	if claimed.UID != job.UID {
		t.Errorf("claimed %s, want %s", claimed.UID, job.UID)
	}
}

// TestAWorkerMayNotReportOnALeaseItHasLost is the rule every write in
// jobs.go shares: a worker whose lease expired has been overtaken, and letting
// it record an outcome would record it over somebody else's work.
func TestAWorkerMayNotReportOnALeaseItHasLost(t *testing.T) {
	f := newJobFixture(t)
	f.enqueue(t, domain.NewJob{Kind: "noop"})

	const lease = time.Minute
	first, ok := f.claim(t, "worker-1", lease)
	if !ok {
		t.Fatal("nothing to claim")
	}

	f.now = f.now.Add(lease + time.Second)
	if _, ok := f.claim(t, "worker-2", lease); !ok {
		t.Fatal("the expired job was not reclaimable")
	}

	outcome := JobOutcome{
		JobID: first.ID, Owner: "worker-1", Now: f.now,
		Event: domain.Event{Type: events.JobCompleted, OccurredAt: f.now},
	}
	if _, err := f.db.CompleteJob(t.Context(), outcome); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("CompleteJob on a lost lease returned %v, want a conflict", err)
	}
	if _, err := f.db.FailJob(t.Context(), outcome, "too late", f.now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("FailJob on a lost lease returned %v, want a conflict", err)
	}
	if _, err := f.db.ReleaseJob(t.Context(), outcome); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("ReleaseJob on a lost lease returned %v, want a conflict", err)
	}
	if err := f.db.ExtendLease(t.Context(), first.ID, "worker-1", f.now.Add(lease)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("ExtendLease on a lost lease returned %v, want a conflict", err)
	}

	// worker-2, which does hold it, may.
	if err := f.db.ExtendLease(t.Context(), first.ID, "worker-2", f.now.Add(2*lease)); err != nil {
		t.Errorf("ExtendLease by the holder: %v", err)
	}
	held, err := f.db.JobByUID(t.Context(), first.UID)
	if err != nil {
		t.Fatalf("JobByUID: %v", err)
	}
	if !held.Leased(f.now.Add(lease + time.Second)) {
		t.Errorf("the extended lease did not outlive the original: %+v", held)
	}
}

// TestCompleteReleasesTheLease keeps a finished row readable in one pass: a
// job that says both "completed" and "held by worker-1" has to be read twice
// to be understood.
func TestCompleteReleasesTheLease(t *testing.T) {
	f := newJobFixture(t)
	f.enqueue(t, domain.NewJob{Kind: "noop"})
	claimed, _ := f.claim(t, "worker-1", time.Minute)

	done, err := f.db.CompleteJob(t.Context(), JobOutcome{
		JobID: claimed.ID, Owner: "worker-1", Now: f.now,
		Event: domain.Event{Type: events.JobCompleted, OccurredAt: f.now},
	})
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if done.CompletedAt.IsZero() {
		t.Error("completed_at was not set")
	}
	if done.LeaseOwner != "" || !done.LeaseExpiresAt.IsZero() {
		t.Errorf("a completed job still holds a lease: %+v", done)
	}
	if got := done.Status(f.now); got != domain.JobCompleted {
		t.Errorf("status = %q, want %q", got, domain.JobCompleted)
	}
	if _, ok := f.claim(t, "worker-2", time.Minute); ok {
		t.Error("a completed job was claimed again")
	}
}

// TestReleaseMakesAJobClaimableAtOnce is the shutdown path's half of
// PLAN.md M6 acceptance 6, from the store's side: releasing gives the job back
// immediately rather than leaving it until the lease expires, and it does not
// forget the attempt that was made.
func TestReleaseMakesAJobClaimableAtOnce(t *testing.T) {
	f := newJobFixture(t)
	f.enqueue(t, domain.NewJob{Kind: "noop"})
	claimed, _ := f.claim(t, "worker-1", time.Hour)

	released, err := f.db.ReleaseJob(t.Context(), JobOutcome{
		JobID: claimed.ID, Owner: "worker-1", Now: f.now,
	})
	if err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}
	if released.LeaseOwner != "" {
		t.Errorf("a released job still names a worker: %+v", released)
	}
	if released.Attempts != 1 {
		t.Errorf("attempts = %d after a release, want the attempt to still count", released.Attempts)
	}
	if !released.FailedAt.IsZero() || !released.CompletedAt.IsZero() {
		t.Errorf("a released job is finished: %+v", released)
	}

	// The same instant, not a lease later.
	if _, ok := f.claim(t, "worker-2", time.Hour); !ok {
		t.Error("a released job was not immediately claimable")
	}
}

// TestStuckLeases is the count "cmsdb check" reports (PLAN.md M6).
func TestStuckLeases(t *testing.T) {
	f := newJobFixture(t)
	f.enqueue(t, domain.NewJob{Kind: "noop"})
	f.enqueue(t, domain.NewJob{Kind: "noop"})

	const lease = time.Minute
	if _, ok := f.claim(t, "worker-1", lease); !ok {
		t.Fatal("nothing to claim")
	}

	report, err := f.db.Check(t.Context(), f.now)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.StuckJobLeases != 0 {
		t.Errorf("stuck leases = %d while the lease is live", report.StuckJobLeases)
	}
	if !report.OK() {
		t.Errorf("a live lease made the check fail: %+v", report)
	}

	report, err = f.db.Check(t.Context(), f.now.Add(lease+time.Second))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.StuckJobLeases != 1 {
		t.Errorf("stuck leases = %d after the lease expired, want 1", report.StuckJobLeases)
	}
	// A stuck lease is a fact, not damage: the queue recovers on its own, and
	// a check that exited non-zero for it would page somebody for an ordinary
	// worker restart.
	if !report.OK() {
		t.Errorf("a stuck lease failed the check: %+v", report)
	}
}

// TestQueryJobs covers the three listings and the order each one promises.
func TestQueryJobs(t *testing.T) {
	f := newJobFixture(t)

	pending := f.enqueue(t, domain.NewJob{Kind: "noop", Priority: domain.PriorityBulk})
	urgent := f.enqueue(t, domain.NewJob{Kind: "other", Priority: domain.PriorityUrgent})

	// One job driven all the way to abandoned.
	doomed := f.enqueue(t, domain.NewJob{Kind: "noop", MaxAttempts: 1})
	claimed, ok := f.claim(t, "worker-1", time.Minute)
	if !ok || claimed.UID != urgent.UID {
		t.Fatalf("claimed %+v, want the urgent job", claimed)
	}
	// Put the urgent one back so that only the doomed job is failed.
	if _, err := f.db.ReleaseJob(t.Context(), JobOutcome{JobID: claimed.ID, Owner: "worker-1", Now: f.now}); err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}
	f.now = f.now.Add(time.Second)
	if _, ok := f.claim(t, "worker-1", time.Minute); !ok {
		t.Fatal("nothing to claim")
	}
	// The urgent job comes back first again; take the doomed one after it.
	for {
		got, ok := f.claim(t, "worker-1", time.Minute)
		if !ok {
			t.Fatal("the doomed job was never claimed")
		}
		if got.UID != doomed.UID {
			continue
		}
		if _, err := f.db.FailJob(t.Context(), JobOutcome{
			JobID: got.ID, Owner: "worker-1", Now: f.now,
			Event: domain.Event{Type: events.JobAbandoned, OccurredAt: f.now},
		}, "boom", f.now); err != nil {
			t.Fatalf("FailJob: %v", err)
		}
		break
	}

	t.Run("failed", func(t *testing.T) {
		got, err := f.db.QueryJobs(t.Context(), domain.JobFilter{Failed: true})
		if err != nil {
			t.Fatalf("QueryJobs: %v", err)
		}
		if len(got) != 1 || got[0].UID != doomed.UID {
			t.Errorf("failed = %v, want just %s", uidsOf(got), doomed.UID)
		}
	})

	t.Run("pending is in the order the workers will take it", func(t *testing.T) {
		got, err := f.db.QueryJobs(t.Context(), domain.JobFilter{Pending: true})
		if err != nil {
			t.Fatalf("QueryJobs: %v", err)
		}
		want := []string{urgent.UID, pending.UID}
		if strings.Join(uidsOf(got), ",") != strings.Join(want, ",") {
			t.Errorf("pending = %v, want %v (priority, then schedule)", uidsOf(got), want)
		}
	})

	t.Run("kind narrows it", func(t *testing.T) {
		got, err := f.db.QueryJobs(t.Context(), domain.JobFilter{Kind: "other"})
		if err != nil {
			t.Fatalf("QueryJobs: %v", err)
		}
		if len(got) != 1 || got[0].UID != urgent.UID {
			t.Errorf("kind=other = %v, want just %s", uidsOf(got), urgent.UID)
		}
	})

	t.Run("a contradiction is refused", func(t *testing.T) {
		if _, err := f.db.QueryJobs(t.Context(), domain.JobFilter{Pending: true, Failed: true}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("pending and failed together returned %v, want invalid", err)
		}
	})
}

func uidsOf(jobs []domain.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.UID)
	}
	return out
}

// TestClaimQueryUsesTheIndex is why 0008 leads jobs_claimable on priority
// rather than on scheduled_for, as DESIGN.md 9 originally wrote it.
//
// It asserts two things about the plan of the statement ClaimJob actually
// runs, rather than of a hand-written approximation of it.
//
// The candidate is found through jobs_claimable and never by reading the
// table. The plan says "SCAN jobs USING INDEX jobs_claimable", and a scan
// that names this index is the pass rather than the failure: nothing
// constrains the leading column, so SQLite walks the index in its own order --
// which is exactly the ORDER BY -- and LIMIT 1 stops it at the first row that
// also satisfies the two time filters. On a queue with work ready that is one
// row. What would be a failure is "SCAN jobs" naming no index at all.
//
// And it does not sort. "USE TEMP B-TREE FOR ORDER BY" is SQLite saying it
// had to materialise and sort the candidates, which is what an index led by
// scheduled_for would force: that index satisfies the range and then has to
// sort the whole ready backlog to honour "ORDER BY priority". Sorting fifty
// thousand ready rows to take one off the front, once per claim, is the cost
// that shows up on the one day the queue is long -- which is the only day it
// matters.
func TestClaimQueryUsesTheIndex(t *testing.T) {
	f := newJobFixture(t)
	f.enqueue(t, domain.NewJob{Kind: "noop"})

	var plan []string
	err := f.db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return run(conn, "explaining the claim", "EXPLAIN QUERY PLAN "+claimSQL,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":owner", "worker-1")
				stmt.SetText(":deadline", formatTime(f.now.Add(time.Minute)))
				stmt.SetText(":now", formatTime(f.now))
			},
			func(stmt *sqlite.Stmt) error {
				plan = append(plan, stmt.GetText("detail"))
				return nil
			})
	})
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("the query plan is empty; nothing was asserted")
	}
	joined := strings.Join(plan, "\n")

	if !strings.Contains(joined, "jobs_claimable") {
		t.Errorf("the claim does not use jobs_claimable:\n%s", joined)
	}
	for _, line := range plan {
		// A scan that names no index reads the table. The alias is the table
		// name here, since the claim's subquery uses none.
		trimmed := strings.TrimSpace(strings.TrimLeft(line, "|-`"))
		if strings.HasPrefix(trimmed, "SCAN jobs") && !strings.Contains(trimmed, "USING") {
			t.Errorf("the claim reads every job row:\n%s", joined)
		}
	}
	if strings.Contains(strings.ToUpper(joined), "TEMP B-TREE") {
		t.Errorf("the claim sorts to honour its ORDER BY; the index does not match it:\n%s", joined)
	}
}
