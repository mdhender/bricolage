// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
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

	engine, err := workflow.New(db, opts.Clock)
	if err != nil {
		return nil, err
	}
	s.engine = engine
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
