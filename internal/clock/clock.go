// Copyright (c) 2026 Michael D Henderson.

// Package clock is the one place outside main that may read the wall clock.
// Every component that needs the time takes a Clock, which is what makes lease
// expiry, scheduling, and due dates testable (invariant 3, DESIGN.md 14).
//
// Permitted imports: the standard library.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time. It is deliberately this small: a component
// that needs to sleep or to wake on a timer takes that as its own seam rather
// than growing this interface, because a Clock that can also schedule is a
// scheduler and belongs to whoever owns the schedule.
type Clock interface {
	Now() time.Time
}

// Real reads the wall clock. It is the only implementation that calls
// time.Now outside of main.
type Real struct{}

// Now returns the current time in UTC. Timestamps are stored as ISO-8601 UTC
// (DESIGN.md 13.5), so there is no reason to hand local time to a caller.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a Clock a test drives by hand. Its zero value is not useful; use
// NewFake, which starts at a fixed, readable instant.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake reading at t, in UTC.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake forward by d and returns the new time.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// Set moves the fake to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

var (
	_ Clock = Real{}
	_ Clock = (*Fake)(nil)
)
