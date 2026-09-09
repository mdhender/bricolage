// Copyright (c) 2026 Michael D Henderson.

package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// The worker loop (DESIGN.md 9, PLAN.md M6).
//
// A worker does one thing in a loop: claim, run, report. Everything else here
// is about the two moments that are not that -- an idle queue, and a process
// going away underneath a job in flight.
//
// An idle queue is polled with backoff. There is no notification channel and
// deliberately so: the queue is a SQLite table that another process may write
// to, so the only signal that would be reliable is the one this already does,
// and a poll that backs off to a couple of seconds costs nothing an idle
// server notices.
//
// A shutdown releases the lease rather than abandoning it
// (PLAN.md M6 acceptance 6). The handler's context is cancelled, the worker
// waits for it to return, and then -- on a context that is deliberately not
// the cancelled one -- it either completes the job, if the handler finished
// anyway, or drops the lease so the next worker finds the job at once instead
// of in a lease's time.

// The idle-poll bounds. The first poll after finding nothing waits
// MinIdleDelay and each empty poll doubles it up to MaxIdleDelay, so a busy
// queue is checked constantly and an idle one is checked twice a second at
// worst.
const (
	MinIdleDelay = 100 * time.Millisecond
	MaxIdleDelay = 2 * time.Second
)

// DefaultWorkers is how many workers "cmsd serve" runs when nobody says
// (DESIGN.md 11, "--workers N, default 1, 0 to disable").
const DefaultWorkers = 1

// PoolOptions configure a Pool. Everything is resolved before New is called;
// this package reads no flags and no environment.
type PoolOptions struct {
	// Queue is required.
	Queue *Queue

	// Registry is required. A pool with no handlers would claim jobs in order
	// to fail them.
	Registry *Registry

	// Workers is how many run concurrently. Zero means DefaultWorkers;
	// negative is an error rather than a silent zero, because "--workers -1"
	// is a typo and a queue that silently stopped running is the worst way to
	// discover it.
	Workers int

	// Name distinguishes this process's workers from another's in
	// jobs.lease_owner. Empty means the host name and the process id, which
	// is what an operator reading a stuck lease needs in order to know which
	// machine to look at.
	Name string

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger

	// Wait is the idle delay, a seam for tests. It returns false when ctx
	// ended before the delay elapsed. A nil Wait uses a timer.
	//
	// It is not a clock.Clock: a Clock reports the time and a component that
	// needs to sleep takes that as its own seam rather than growing the
	// interface everything else shares (internal/clock).
	Wait func(ctx context.Context, d time.Duration) bool
}

// Pool runs the workers hosted inside cmsd.
type Pool struct {
	queue    *Queue
	registry *Registry
	workers  int
	name     string
	log      *slog.Logger
	wait     func(ctx context.Context, d time.Duration) bool
}

// NewPool builds a worker pool.
func NewPool(opts PoolOptions) (*Pool, error) {
	if opts.Queue == nil {
		return nil, fmt.Errorf("jobs: no queue")
	}
	if opts.Registry == nil {
		return nil, fmt.Errorf("jobs: no handler registry")
	}
	if opts.Workers < 0 {
		return nil, fmt.Errorf("jobs: %d workers: not a count", opts.Workers)
	}

	p := &Pool{
		queue:    opts.Queue,
		registry: opts.Registry,
		workers:  opts.Workers,
		name:     strings.TrimSpace(opts.Name),
		log:      opts.Logger,
		wait:     opts.Wait,
	}
	if p.workers == 0 {
		p.workers = DefaultWorkers
	}
	if p.name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		p.name = fmt.Sprintf("%s/%d", host, os.Getpid())
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.wait == nil {
		p.wait = sleep
	}
	return p, nil
}

// Workers is how many workers Run will start.
func (p *Pool) Workers() int { return p.workers }

// Run starts the workers and returns when ctx ends and every one of them has
// stopped.
//
// It returns nil on an ordinary shutdown. A worker never returns an error: a
// job that fails is recorded on the job, and a queue that could not be read is
// logged and retried, because the alternative -- one unreadable row taking the
// server down -- is worse than the outage it was trying to report.
//
// Run blocking until the workers have stopped is the contract that makes
// invariant 17 hold with workers in the picture: internal/server drains HTTP,
// then cancels this, then waits here, and there is still one shutdown path.
func (p *Pool) Run(ctx context.Context) error {
	if p.workers == 0 {
		return nil
	}
	p.log.Info("job workers starting", "workers", p.workers, "name", p.name, "kinds", p.registry.Kinds())

	var wg sync.WaitGroup
	for i := range p.workers {
		worker := fmt.Sprintf("%s#%d", p.name, i+1)
		wg.Go(func() { p.loop(ctx, worker) })
	}
	wg.Wait()

	p.log.Info("job workers stopped", "workers", p.workers, "name", p.name)
	return nil
}

// loop is one worker: claim, run, report, until ctx ends.
func (p *Pool) loop(ctx context.Context, worker string) {
	idle := MinIdleDelay
	for {
		if ctx.Err() != nil {
			return
		}

		job, ok, err := p.queue.Claim(ctx, worker)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				// The claim was interrupted by the shutdown, not refused.
				return
			}
			// Logged and retried. A queue this process cannot read is a
			// problem worth saying out loud once per poll, and it is not a
			// reason to stop serving HTTP.
			p.log.Error("claiming a job failed", "worker", worker, "error", err)
			if !p.backOff(ctx, &idle) {
				return
			}
			continue
		case !ok:
			if !p.backOff(ctx, &idle) {
				return
			}
			continue
		}

		idle = MinIdleDelay
		p.execute(ctx, worker, job)
	}
}

// backOff waits out the idle delay and doubles it, reporting false when ctx
// ended first.
func (p *Pool) backOff(ctx context.Context, idle *time.Duration) bool {
	if !p.wait(ctx, *idle) {
		return false
	}
	if *idle *= 2; *idle > MaxIdleDelay {
		*idle = MaxIdleDelay
	}
	return true
}

// execute runs one claimed job and records what became of it.
//
// The reporting context is deliberately not ctx. By the time a job finishes
// during a shutdown, ctx is already cancelled, and reporting the outcome
// through it would mean the outcome was never written -- the job would sit
// leased until it expired, which is exactly the failure mode the lease exists
// to bound and not one to reintroduce at the moment of shutdown.
func (p *Pool) execute(ctx context.Context, worker string, job domain.Job) {
	started := p.queue.Now()
	err := p.registry.dispatch(ctx, job)

	report, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
	defer cancel()

	// A handler that returned an error while the process was shutting down
	// was most likely interrupted rather than broken. The lease goes back
	// rather than the attempt being called a failure: the job is somebody
	// else's now, and it is claimable immediately (PLAN.md M6 acceptance 6).
	if err != nil && ctx.Err() != nil {
		p.log.Info("releasing an in-flight job at shutdown",
			"worker", worker, "job", job.UID, "kind", job.Kind)
		if rerr := p.queue.Release(report, job, worker); rerr != nil && !leaseLost(rerr) {
			p.log.Error("releasing a job failed", "worker", worker, "job", job.UID, "error", rerr)
		}
		return
	}

	if err != nil {
		p.log.Warn("job attempt failed",
			"worker", worker, "job", job.UID, "kind", job.Kind,
			"attempt", job.Attempts, "max_attempts", job.MaxAttempts, "error", err)
		if ferr := p.queue.Fail(report, job, worker, err); ferr != nil {
			p.logLeaseOutcome(worker, job, "recording a failure", ferr)
		}
		return
	}

	if cerr := p.queue.Complete(report, job, worker); cerr != nil {
		p.logLeaseOutcome(worker, job, "recording a completion", cerr)
		return
	}
	p.log.Info("job completed",
		"worker", worker, "job", job.UID, "kind", job.Kind,
		"attempt", job.Attempts, "took", p.queue.Now().Sub(started).String())
}

// logLeaseOutcome writes the right line for a write that the store refused.
//
// A lost lease is not an error in this process: the worker was overtaken, and
// whoever holds the job now will report on it. Anything else is.
func (p *Pool) logLeaseOutcome(worker string, job domain.Job, what string, err error) {
	if leaseLost(err) {
		p.log.Warn("the lease was lost before "+what,
			"worker", worker, "job", job.UID, "kind", job.Kind, "error", errors.Join(ErrLeaseLost, err))
		return
	}
	p.log.Error(what+" failed", "worker", worker, "job", job.UID, "error", err)
}

// reportTimeout bounds the write that records an outcome after ctx has been
// cancelled. It is generous: the write is one small transaction, and a
// shutdown that gave up on it would leave the lease held.
const reportTimeout = 10 * time.Second

// sleep is the default idle wait. It reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
