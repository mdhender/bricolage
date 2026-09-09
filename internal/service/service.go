// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/jobs"
	"github.com/mdhender/bricolage/internal/publish"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/store"
	"github.com/mdhender/bricolage/internal/workflow"
)

// Service is the use-case layer: it owns transactions, writes events, and is
// the only thing above the store that decides anything (DESIGN.md 3).
//
// It holds a clock rather than reading one, which is what makes session expiry
// and lease timing testable at an arbitrary instant (invariant 3).
type Service struct {
	db    *store.DB
	clock clock.Clock
	log   *slog.Logger

	// sessionTTL is how long a session issued here lasts.
	sessionTTL time.Duration

	// touchAfter is how stale a session's last_seen_at may get before
	// authenticating it writes a new one. Recording every request would put a
	// write on the single write connection in front of every read.
	touchAfter time.Duration

	// lockLease is how long a document's edit lease lasts. It is a duration
	// rather than a flag on the row because the lock is a lease: activity
	// extends it and time ends it (DESIGN.md 5.1).
	lockLease time.Duration

	// engine moves documents between states. It is held rather than built per
	// call so that there is one of it, and it is reached only through the two
	// methods below: nothing else in this package may write documents.state,
	// and the way to be sure of that is to have no other way to try
	// (invariant 4).
	engine *workflow.Engine

	// queue is the job queue (PLAN.md M6). It is held rather than built per
	// call so that there is one of it in the process: the worker pool and the
	// API both work through this one, which is what makes "when does this
	// lease expire" have a single answer.
	queue *jobs.Queue

	// queues are the saved queue definitions (PLAN.md M5). They are
	// configuration rather than schema: a queue has no identity anybody
	// refers to and nothing points at one, so naming a few is a config file's
	// job and not a migration's.
	queues config.QueueSet

	// renderer is the template tree and preview is the scratch tree
	// (PLAN.md M8). Both are nil on a server started without them, which is a
	// supported configuration and not a mistake: M0 through M6 is a working
	// editorial system with no publishing, and an installation that does its
	// rendering somewhere else should not have to invent two directories to
	// start the server.
	renderer *render.Engine
	preview  *render.Scratch

	// publisher renders to the output tree and remembers what it wrote
	// (PLAN.md M9). It is nil on a server started without --output or
	// without --templates, which is a supported configuration: M0 through M8
	// is a working editorial system that renders previews and publishes
	// nothing, and a request to publish on such a server is a 503 naming the
	// flag it was not given.
	publisher *publish.Publisher
}

// Options configure a Service. Everything is resolved before New is called;
// this package reads no flags, no environment, and no file.
type Options struct {
	// Clock is required. There is no default, deliberately: a service that
	// silently falls back to the wall clock is a service whose tests pass for
	// the wrong reason.
	Clock clock.Clock

	// Logger receives the structured log. A nil Logger discards.
	Logger *slog.Logger

	// SessionTTL is how long a session lasts; zero means
	// config.DefaultSessionTTL.
	SessionTTL time.Duration

	// TouchAfter is the staleness threshold for last_seen_at; zero means one
	// minute.
	TouchAfter time.Duration

	// LockLease is how long a document's edit lease lasts; zero means
	// config.DefaultLockLease.
	LockLease time.Duration

	// Queues are the saved queue definitions; an empty set means
	// config.DefaultQueues.
	Queues config.QueueSet

	// JobLease is how long a worker's claim on a job lasts; zero means
	// jobs.DefaultLease.
	JobLease time.Duration

	// Renderer is the template tree, or nil for a server that renders
	// nothing. Preview is the scratch tree previews are written to, or nil
	// for a server that serves none. Both are built by main, from a directory
	// that must already exist, because opening one is where the failure
	// belongs: a server that could not read its templates should say so while
	// starting rather than on the first preview.
	Renderer *render.Engine
	Preview  *render.Scratch

	// Publisher writes to the output tree, or nil for a server that
	// publishes nothing. It is built by main, over a directory that must
	// already exist, for the reason the other two are.
	Publisher *publish.Publisher
}

// DefaultTouchAfter is how stale last_seen_at may get before authentication
// refreshes it.
const DefaultTouchAfter = time.Minute

// New builds a Service over an open database.
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("service: no database")
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("service: no clock; every component that needs the time is given one (invariant 3)")
	}
	s := &Service{
		db:         db,
		clock:      opts.Clock,
		log:        opts.Logger,
		sessionTTL: opts.SessionTTL,
		touchAfter: opts.TouchAfter,
		lockLease:  opts.LockLease,
		queues:     opts.Queues,
		renderer:   opts.Renderer,
		preview:    opts.Preview,
		publisher:  opts.Publisher,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.sessionTTL <= 0 {
		s.sessionTTL = config.DefaultSessionTTL
	}
	if s.touchAfter <= 0 {
		s.touchAfter = DefaultTouchAfter
	}
	if s.lockLease <= 0 {
		s.lockLease = config.DefaultLockLease
	}
	if s.queues.IsEmpty() {
		s.queues = config.DefaultQueues()
	}

	engine, err := workflow.New(db, opts.Clock)
	if err != nil {
		return nil, err
	}
	s.engine = engine

	queue, err := jobs.NewQueue(db, opts.Clock, opts.JobLease)
	if err != nil {
		return nil, err
	}
	s.queue = queue
	return s, nil
}

// DB returns the open database. It is here for the composition root, which
// owns closing it, and for tests that assert on rows the service wrote.
func (s *Service) DB() *store.DB { return s.db }

// Now reads the injected clock. Nothing in this package calls time.Now
// (invariant 3).
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// SessionTTL is how long a session issued by this service lasts.
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// LockLease is how long a document's edit lease lasts.
func (s *Service) LockLease() time.Duration { return s.lockLease }
