// Copyright (c) 2026 Michael D Henderson.

package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/store"
)

// The worker loop's tests (PLAN.md M6). They run against a real in-memory
// database with every migration applied, because the thing being tested is
// what happens between a claim and an outcome and a mock of the queue would be
// a mock of exactly that (AGENTS.md, "Testing").

type fixture struct {
	db    *store.DB
	clock *clock.Fake
	queue *Queue
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	q, err := NewQueue(db, c, time.Minute)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	return &fixture{db: db, clock: c, queue: q}
}

func (f *fixture) enqueue(t *testing.T, n domain.NewJob) domain.Job {
	t.Helper()
	job, err := f.queue.Enqueue(t.Context(), n)
	if err != nil {
		t.Fatalf("Enqueue(%+v): %v", n, err)
	}
	return job
}

func (f *fixture) reload(t *testing.T, uid string) domain.Job {
	t.Helper()
	job, err := f.queue.Job(t.Context(), uid)
	if err != nil {
		t.Fatalf("Job(%s): %v", uid, err)
	}
	return job
}

// eventTypes returns a job's history, oldest first, which is the order it
// happened in.
func (f *fixture) eventTypes(t *testing.T, job domain.Job) []string {
	t.Helper()
	history, err := f.db.EventsForSubject(t.Context(), domain.SubjectJob, job.ID, 50)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	out := make([]string, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		out = append(out, history[i].Type)
	}
	return out
}

// TestEnqueueAndClaimThroughTheQueue is the round trip every worker makes,
// with the events the operations write (invariant 7).
func TestEnqueueAndClaimThroughTheQueue(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop})

	claimed, ok, err := f.queue.Claim(t.Context(), "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v; want the job that was just enqueued", ok, err)
	}
	if err := f.queue.Complete(t.Context(), claimed, "worker-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	want := []string{events.JobEnqueued, events.JobCompleted}
	if got := f.eventTypes(t, job); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("history = %v, want %v", got, want)
	}

	// A claim does not write an event, which is the decision recorded in
	// internal/events: a lease is a deadline that expires on its own, and the
	// row carries all of it.
	for _, e := range f.eventTypes(t, job) {
		if strings.Contains(e, "claim") {
			t.Errorf("a claim wrote an event (%s); the lease is on the row", e)
		}
	}
}

// TestFailThenAbandon is the backoff and the two different events. "It is
// being retried" and "nobody is going to try again" are the two things a
// person reading a queue needs to tell apart.
func TestFailThenAbandon(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop, MaxAttempts: 2})
	boom := errors.New("the thing it depends on is down")

	first, ok, err := f.queue.Claim(t.Context(), "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	if err := f.queue.Fail(t.Context(), first, "worker-1", boom); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	after := f.reload(t, job.UID)
	if !after.FailedAt.IsZero() {
		t.Error("the first of two attempts abandoned the job")
	}
	if after.LastError != boom.Error() {
		t.Errorf("last_error = %q, want %q", after.LastError, boom.Error())
	}
	// The retry waits: retrying at once is how one outage becomes five
	// failures in the same second.
	want := f.clock.Now().Add(domain.RetryDelay(1))
	if !after.ScheduledFor.Equal(want) {
		t.Errorf("scheduled_for = %v, want %v (one backoff away)", after.ScheduledFor, want)
	}
	if _, ok, _ := f.queue.Claim(t.Context(), "worker-2"); ok {
		t.Error("the job was retried before its backoff elapsed")
	}

	f.clock.Set(want)
	second, ok, err := f.queue.Claim(t.Context(), "worker-2")
	if err != nil || !ok {
		t.Fatalf("the retry was not claimable: %v, %v", ok, err)
	}
	if err := f.queue.Fail(t.Context(), second, "worker-2", boom); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	abandoned := f.reload(t, job.UID)
	if abandoned.FailedAt.IsZero() {
		t.Fatalf("the last of two attempts did not abandon the job: %+v", abandoned)
	}

	wantEvents := []string{events.JobEnqueued, events.JobFailed, events.JobAbandoned}
	if got := f.eventTypes(t, job); strings.Join(got, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("history = %v, want %v", got, wantEvents)
	}

	// And the retry puts it back, recording the failure it came out of.
	retried, err := f.queue.Retry(t.Context(), abandoned, 0)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.Attempts != 0 || !retried.FailedAt.IsZero() {
		t.Errorf("a retried job is not pending: %+v", retried)
	}
	if got := f.eventTypes(t, job); got[len(got)-1] != events.JobRetried {
		t.Errorf("the newest event is %q, want %q", got[len(got)-1], events.JobRetried)
	}
}

// TestRegistry is the handler lookup, including the case a half-finished
// deployment produces.
func TestRegistry(t *testing.T) {
	r := NewRegistry()

	if _, ok := r.Handler(KindNoop); !ok {
		t.Fatalf("the noop kind is not registered; the kinds are %v", r.Kinds())
	}
	if err := r.dispatch(t.Context(), domain.Job{Kind: KindNoop}); err != nil {
		t.Errorf("the noop handler returned %v", err)
	}

	// A kind with no handler is an ordinary job failure rather than a panic:
	// the row is real, somebody enqueued it, and a binary rolled back past
	// the kind that enqueued the job recovers when it is rolled forward.
	err := r.dispatch(t.Context(), domain.Job{Kind: "publish"})
	if err == nil {
		t.Fatal("dispatching an unregistered kind succeeded")
	}
	if !strings.Contains(err.Error(), "publish") || !strings.Contains(err.Error(), KindNoop) {
		t.Errorf("the error is %q; it should name the kind and what this binary knows", err)
	}

	r.Register("publish", HandlerFunc(func(context.Context, domain.Job) error { return nil }))
	if err := r.dispatch(t.Context(), domain.Job{Kind: "publish"}); err != nil {
		t.Errorf("after registering, dispatch returned %v", err)
	}
	if got := strings.Join(r.Kinds(), ","); got != "noop,publish" {
		t.Errorf("Kinds() = %q, want them sorted", got)
	}
}

// newPool builds a pool over the fixture with a handler the test drives.
func (f *fixture) newPool(t *testing.T, workers int, h Handler) *Pool {
	t.Helper()
	r := NewRegistry()
	r.Register(KindNoop, h)
	p, err := NewPool(PoolOptions{
		Queue:    f.queue,
		Registry: r,
		Workers:  workers,
		Name:     "test",
		// No idle delay: the loop polls as fast as it can, so a test that
		// waits for a job to run waits milliseconds rather than seconds.
		Wait: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

// TestThePoolRunsAJob is the loop end to end: a job enqueued before the
// workers start is claimed, run, and completed, and the pool stops when its
// context does.
func TestThePoolRunsAJob(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop})

	ran := make(chan struct{})
	pool := f.newPool(t, 2, HandlerFunc(func(context.Context, domain.Job) error {
		close(ran)
		return nil
	}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop when its context ended")
	}

	after := f.reload(t, job.UID)
	if after.CompletedAt.IsZero() {
		t.Errorf("the job was not completed: %+v", after)
	}
	if after.LeaseOwner != "" {
		t.Errorf("a completed job still holds a lease: %+v", after)
	}
}

// TestAFailingHandlerIsRecordedAndRetried is the pool's half of the failure
// path: the handler's error becomes last_error, and the job comes back.
func TestAFailingHandlerIsRecordedAndRetried(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop, MaxAttempts: 3})

	failed := make(chan struct{})
	var once sync.Once
	pool := f.newPool(t, 1, HandlerFunc(func(context.Context, domain.Job) error {
		once.Do(func() { close(failed) })
		return errors.New("handler said no")
	}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	// The handler signals on the way in, so the outcome is written after it
	// returns; wait for the row rather than for the signal. The clock never
	// advances, so the backoff keeps the job out of reach and the loop cannot
	// run it a second time while this waits.
	var after domain.Job
	deadline := time.After(5 * time.Second)
	for {
		after = f.reload(t, job.UID)
		if after.Attempts == 1 && after.LastError != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the failure was never recorded: %+v", after)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if after.Attempts != 1 {
		t.Errorf("attempts = %d after one failed run, want 1", after.Attempts)
	}
	if after.LastError != "handler said no" {
		t.Errorf("last_error = %q, want the handler's error", after.LastError)
	}
	if !after.FailedAt.IsZero() {
		t.Errorf("one failure of three abandoned the job: %+v", after)
	}
}

// TestShutdownReleasesTheLease is PLAN.md M6 acceptance 6: graceful shutdown
// releases the lease of an in-flight job rather than abandoning it.
//
// "Abandoning it" is the failure being tested for, and it has two shapes. The
// job must not be left leased -- which is what walking away from it would do,
// and would make it invisible for a whole lease -- and it must not be recorded
// as a failure, because nothing failed: the process was asked to stop.
func TestShutdownReleasesTheLease(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop})

	running := make(chan struct{})
	pool := f.newPool(t, 1, HandlerFunc(func(ctx context.Context, _ domain.Job) error {
		close(running)
		// A handler that honours cancellation, which is the contract:
		// shutdown reaches a job in flight by cancelling its context.
		<-ctx.Done()
		return ctx.Err()
	}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop; it is waiting for something")
	}

	after := f.reload(t, job.UID)
	if after.LeaseOwner != "" || !after.LeaseExpiresAt.IsZero() {
		t.Errorf("the lease was abandoned rather than released: %+v", after)
	}
	if !after.FailedAt.IsZero() || after.LastError != "" {
		t.Errorf("an interrupted job was recorded as a failure: %+v", after)
	}
	if !after.CompletedAt.IsZero() {
		t.Errorf("an interrupted job was recorded as completed: %+v", after)
	}

	// The point of releasing rather than abandoning: the next worker to look
	// finds it now, not in a lease's time.
	if !after.Claimable(f.clock.Now()) {
		t.Errorf("the released job is not immediately claimable: %+v", after)
	}
	if _, ok, err := f.queue.Claim(t.Context(), "the-next-process"); err != nil || !ok {
		t.Errorf("Claim after the shutdown = %v, %v; want the released job", ok, err)
	}

	// The attempt still counts. A queue that forgot attempts it had made
	// would let a job that reliably kills its worker run forever.
	if after.Attempts != 1 {
		t.Errorf("attempts = %d after a released attempt, want 1", after.Attempts)
	}
}

// TestShutdownLetsAFinishedJobFinish is the other half of DESIGN.md 11's
// sentence: "let workers finish the current job or release its lease". A
// handler that completes despite the cancellation is completed, not released.
func TestShutdownLetsAFinishedJobFinish(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(t, domain.NewJob{Kind: KindNoop})

	running := make(chan struct{})
	pool := f.newPool(t, 1, HandlerFunc(func(ctx context.Context, _ domain.Job) error {
		close(running)
		<-ctx.Done()
		// Finished anyway. The outcome is written on a context that is
		// deliberately not the cancelled one, which is the only reason this
		// can be recorded at all.
		return nil
	}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	<-running
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not stop")
	}

	after := f.reload(t, job.UID)
	if after.CompletedAt.IsZero() {
		t.Errorf("a job that finished during the shutdown was not completed: %+v", after)
	}
}

// TestWorkersDoNotRunAJobTwice is the claim's exactly-once property seen from
// the loop: several workers over several jobs, each job run once.
//
// Run under -race, where it earns its keep.
func TestWorkersDoNotRunAJobTwice(t *testing.T) {
	f := newFixture(t)

	const jobCount = 25
	uids := make(map[string]bool, jobCount)
	for range jobCount {
		uids[f.enqueue(t, domain.NewJob{Kind: KindNoop}).UID] = true
	}

	var (
		mu   sync.Mutex
		seen = make(map[string]int, jobCount)
		runs atomic.Int64
	)
	pool := f.newPool(t, 8, HandlerFunc(func(_ context.Context, job domain.Job) error {
		mu.Lock()
		seen[job.UID]++
		mu.Unlock()
		runs.Add(1)
		return nil
	}))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for runs.Load() < jobCount {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d jobs ran", runs.Load(), jobCount)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for uid := range uids {
		switch seen[uid] {
		case 0:
			t.Errorf("job %s never ran", uid)
		case 1:
		default:
			t.Errorf("job %s ran %d times; the claim is meant to be exactly once", uid, seen[uid])
		}
	}
}

// TestExtendLeaseKeepsAJobItsOwner is the heartbeat DESIGN.md 9 requires of a
// long job, and the reason it exists: without it, a job outliving its lease is
// claimed out from under the worker still running it.
func TestExtendLeaseKeepsAJobItsOwner(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, domain.NewJob{Kind: KindNoop})

	claimed, ok, err := f.queue.Claim(t.Context(), "slow-worker")
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}

	// Past the original lease, and the job would be anybody's.
	f.clock.Advance(f.queue.Lease() + time.Second)
	if err := f.queue.ExtendLease(t.Context(), claimed, "slow-worker"); err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}
	if _, ok, _ := f.queue.Claim(t.Context(), "somebody-else"); ok {
		t.Error("a heartbeat did not keep the job its owner's")
	}

	// And the holder can still finish it.
	if err := f.queue.Complete(t.Context(), claimed, "slow-worker"); err != nil {
		t.Errorf("Complete after a heartbeat: %v", err)
	}
}

// TestNewPoolRefusesNonsense is the wiring the composition root depends on.
func TestNewPoolRefusesNonsense(t *testing.T) {
	f := newFixture(t)
	r := NewRegistry()

	if _, err := NewPool(PoolOptions{Registry: r}); err == nil {
		t.Error("a pool with no queue was accepted")
	}
	if _, err := NewPool(PoolOptions{Queue: f.queue}); err == nil {
		t.Error("a pool with no handler registry was accepted")
	}
	if _, err := NewPool(PoolOptions{Queue: f.queue, Registry: r, Workers: -1}); err == nil {
		t.Error("a negative worker count was accepted")
	}

	// Zero means the default rather than none: --workers 0 is handled by the
	// composition root, which builds no pool at all.
	p, err := NewPool(PoolOptions{Queue: f.queue, Registry: r})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if p.Workers() != DefaultWorkers {
		t.Errorf("Workers() = %d, want the default %d", p.Workers(), DefaultWorkers)
	}
}

// TestNewQueueRefusesNoClock is invariant 3 at the constructor: a component
// that silently fell back to the wall clock is a component whose tests pass
// for the wrong reason.
func TestNewQueueRefusesNoClock(t *testing.T) {
	f := newFixture(t)
	if _, err := NewQueue(f.db, nil, time.Minute); err == nil {
		t.Error("a queue with no clock was accepted")
	}
	if _, err := NewQueue(nil, f.clock, time.Minute); err == nil {
		t.Error("a queue with no database was accepted")
	}
	q, err := NewQueue(f.db, f.clock, 0)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	if q.Lease() != DefaultLease {
		t.Errorf("Lease() = %s, want the default %s", q.Lease(), DefaultLease)
	}
}

// TestEnqueueRefusesAnInvalidJob keeps the validation on the way in rather
// than once per attempt, five attempts later, in a worker log.
func TestEnqueueRefusesAnInvalidJob(t *testing.T) {
	f := newFixture(t)
	for name, n := range map[string]domain.NewJob{
		"no kind":      {},
		"bad priority": {Kind: KindNoop, Priority: 9},
		"bad payload":  {Kind: KindNoop, Payload: "["},
		"no attempts":  {Kind: KindNoop, MaxAttempts: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.queue.Enqueue(t.Context(), n); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("Enqueue(%+v) = %v, want invalid", n, err)
			}
		})
	}
}
