// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/jobs"
)

// The job queue's use cases (PLAN.md M6).
//
// There are two of them and they are both administrative: look at the queue,
// and put an abandoned job back on it. Enqueuing is not here, because nothing
// a person does is "enqueue a job" -- they publish something, or they expire
// something, and the milestone that adds that operation adds the Enqueue call
// inside it. An API route that let a client post an arbitrary payload under an
// arbitrary kind would be a way to run any handler in the binary with
// arguments the client chose.
//
// The privilege is resolved against the system subject, which is the zero
// Subject: a scope that constrains nothing matches it, and a scope naming a
// site or a category does not. That is the right answer rather than a
// convenient one. The queue is not on a site and not in a category; a
// site-scoped editor's grant says what they may do to that site's documents
// and says nothing at all about the process that publishes them, so it must
// not reach this.

// SystemSubject is what a system-wide privilege is resolved against.
//
// Every field is zero, so authz.Matches admits only a grant that constrains
// nothing -- a global grant, which is what "cmsdb seed" gives the four default
// roles and what an administrator writes deliberately. There is no separate
// "may operate the queue" privilege and there should not be: the scale is
// ordered and cumulative (DESIGN.md 7), and a level whose only difference from
// its neighbour is one screen would be a level nobody could explain.
var SystemSubject = domain.Subject{}

// Jobs returns the jobs matching a filter (PLAN.md M6).
//
// Reading the queue needs Read over the system subject. There is no per-row
// filtering the way ListDocuments has one: a job names no site and no
// category, so there is nothing to resolve it against, and somebody who may
// see the queue sees the queue.
func (s *Service) Jobs(ctx context.Context, actor domain.Identity, f domain.JobFilter) ([]domain.Job, error) {
	if err := s.maySeeJobs(actor); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return s.queue.List(ctx, f)
}

// Job returns one job by uid (invariant 10).
func (s *Service) Job(ctx context.Context, actor domain.Identity, uid string) (domain.Job, error) {
	if err := s.maySeeJobs(actor); err != nil {
		return domain.Job{}, err
	}
	return s.queue.Job(ctx, uid)
}

// RetryJob puts an abandoned job back on the queue (PLAN.md M6).
//
// It needs Publish over the system subject, which is the top of the scale and,
// in a seeded database, the administrator alone. A job's payload is whatever
// the operation that enqueued it decided, and running it again runs that
// operation again -- a publish, in M9. The person who may do that is the
// person who may publish anywhere, and requiring less would make the retry
// button a way around the privilege the original operation needed.
//
// A job that has not failed is a conflict rather than a no-op: "retry" said of
// something still running is a request made under a mistaken belief, and
// answering it with a cheerful 200 is how the mistake survives.
func (s *Service) RetryJob(ctx context.Context, actor domain.Identity, uid string) (domain.Job, error) {
	if err := s.maySeeJobs(actor); err != nil {
		return domain.Job{}, err
	}
	if !authz.Allows(actor.Grants, SystemSubject, domain.Publish) {
		return domain.Job{}, fmt.Errorf(
			"job %s: %s over everything is required to retry a job: %w",
			uid, domain.Publish, domain.ErrForbidden)
	}

	job, err := s.queue.Job(ctx, uid)
	if err != nil {
		return domain.Job{}, err
	}
	return s.queue.Retry(ctx, job, actor.User.ID)
}

// EnqueueJob schedules work.
//
// It takes no actor and performs no check, because it is not something a
// client asks for: it is what a service method calls once it has already
// decided that the operation it is performing is allowed. The milestone that
// adds an operation with a background half adds the call inside that
// operation, where the privilege has been resolved against the document the
// work is about.
func (s *Service) EnqueueJob(ctx context.Context, n domain.NewJob) (domain.Job, error) {
	return s.queue.Enqueue(ctx, n)
}

// JobQueue returns the job queue, for the composition root that builds the
// worker pool over it. There is one per process for the same reason there is
// one clock: two would be two answers to "when does this lease expire".
//
// It is not called Queue because Queue is already taken, by the saved document
// queues of M5. The word means two things in this system -- a named question
// about documents, and the background work table -- and the method names say
// which each time rather than leaving it to the reader.
func (s *Service) JobQueue() *jobs.Queue { return s.queue }

// maySeeJobs resolves the privilege reading the queue needs.
//
// A caller without it is told the queue is not there rather than that they may
// not see it, matching what mayDo does for a document: the existence of the
// route is public, the existence of work is not.
func (s *Service) maySeeJobs(actor domain.Identity) error {
	if !authz.Allows(actor.Grants, SystemSubject, domain.Read) {
		return fmt.Errorf("the job queue: %w", domain.ErrNotFound)
	}
	return nil
}
