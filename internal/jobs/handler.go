// Copyright (c) 2026 Michael D Henderson.

package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/mdhender/bricolage/internal/domain"
)

// KindNoop is a job kind that does nothing successfully.
//
// It exists because a queue with no job kinds cannot be exercised, and a queue
// nobody exercises is a queue whose first real kind discovers the bugs
// (PLAN.md M6, "A noop job kind for testing"). It is registered by default so
// that "earl job list" against a freshly started server has something to show
// and the worker loop has something to run.
const KindNoop = "noop"

// Handler runs one job.
//
// It takes a context and honours cancellation, because that is how graceful
// shutdown reaches a job in flight: the process is going away, the handler is
// asked to stop, and the worker releases the lease so somebody else can pick
// the job up (PLAN.md M6 acceptance 6).
//
// It returns an error and nothing else. What a job produces it produces by
// writing to the database or the output tree; a return value nobody stores is
// a return value that would have to be invented a place to go.
type Handler interface {
	Handle(ctx context.Context, job domain.Job) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, job domain.Job) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, job domain.Job) error { return f(ctx, job) }

// Registry maps a job kind to the handler that runs it.
//
// It is keyed by the kind string that is stored in the row, so a job enqueued
// by one version of the binary and claimed by another names its handler by a
// name rather than by a code pointer. A kind with no handler is not a crash:
// it is a job that fails with an error naming the kind and the kinds this
// binary knows, which is what a half-finished deployment looks like from the
// outside and is worth saying out loud.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns a registry with the kinds every binary carries.
//
// Today that is the noop kind alone. Publish and expire arrive in M9, from the
// package that owns them; a kind registered before its handler exists would be
// a name that lies about what the queue can run (invariant 6 in spirit).
func NewRegistry() *Registry {
	r := &Registry{handlers: make(map[string]Handler)}
	r.Register(KindNoop, HandlerFunc(func(context.Context, domain.Job) error { return nil }))
	return r
}

// Register adds a handler for a kind, replacing any handler already there.
//
// Replacing rather than refusing is deliberate and narrow: a test substitutes
// a handler for a kind it wants to watch, and a registry that refused would
// make that a second registry with a different set of kinds in it. Production
// wiring registers each kind once, from one place.
func (r *Registry) Register(kind string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

// Handler returns the handler for a kind.
func (r *Registry) Handler(kind string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[kind]
	return h, ok
}

// Kinds returns every registered kind, sorted, for the error message that
// names them and for the log line at startup.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for kind := range r.handlers {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// dispatch runs the handler for a job, or reports that there is none.
//
// A missing handler is an ordinary job failure rather than a panic: the row is
// real, somebody enqueued it, and the queue's answer to work it cannot do is
// to retry it a few times and then say so in last_error. A binary rolled back
// past the kind that enqueued the job is exactly this case, and it recovers
// when the binary is rolled forward again.
func (r *Registry) dispatch(ctx context.Context, job domain.Job) error {
	h, ok := r.Handler(job.Kind)
	if !ok {
		return fmt.Errorf("no handler for job kind %q (this binary knows %v)", job.Kind, r.Kinds())
	}
	return h.Handle(ctx, job)
}
