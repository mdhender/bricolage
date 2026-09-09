// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The queue's pure rules (PLAN.md M6). domain is pure, so it is tested
// exhaustively and table-driven (AGENTS.md, "Testing").

func jobTime() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// TestJobStatus is the derivation that replaces a status column. A status
// column would be a second statement of what completed_at, failed_at, and the
// lease already say, and the two would disagree the first time a lease expired
// without anybody writing a row -- which is the case this queue exists to
// survive.
func TestJobStatus(t *testing.T) {
	now := jobTime()

	for name, tc := range map[string]struct {
		job  Job
		want JobStatus
	}{
		"fresh":                      {Job{ScheduledFor: now}, JobPending},
		"scheduled for later":        {Job{ScheduledFor: now.Add(time.Hour)}, JobPending},
		"leased":                     {Job{LeaseOwner: "w1", LeaseExpiresAt: now.Add(time.Minute)}, JobRunning},
		"lease expired":              {Job{LeaseOwner: "w1", LeaseExpiresAt: now.Add(-time.Minute)}, JobPending},
		"lease expiring exactly now": {Job{LeaseOwner: "w1", LeaseExpiresAt: now}, JobPending},
		"completed":                  {Job{CompletedAt: now}, JobCompleted},
		"failed":                     {Job{FailedAt: now}, JobFailed},
		"completed while leased":     {Job{CompletedAt: now, LeaseOwner: "w1", LeaseExpiresAt: now.Add(time.Hour)}, JobCompleted},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.job.Status(now); got != tc.want {
				t.Errorf("Status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJobClaimable is the Go statement of the SQL predicate in
// store.ClaimJob. The two are asserted against each other by the store's
// tests; this one says what the rule is.
func TestJobClaimable(t *testing.T) {
	now := jobTime()

	for name, tc := range map[string]struct {
		job  Job
		want bool
	}{
		"ready":                    {Job{ScheduledFor: now.Add(-time.Hour)}, true},
		"due exactly now":          {Job{ScheduledFor: now}, true},
		"one second early":         {Job{ScheduledFor: now.Add(time.Second)}, false},
		"leased":                   {Job{ScheduledFor: now, LeaseOwner: "w1", LeaseExpiresAt: now.Add(time.Minute)}, false},
		"lease expired":            {Job{ScheduledFor: now, LeaseOwner: "w1", LeaseExpiresAt: now.Add(-time.Second)}, true},
		"completed":                {Job{ScheduledFor: now, CompletedAt: now}, false},
		"failed":                   {Job{ScheduledFor: now, FailedAt: now}, false},
		"failed and lease expired": {Job{ScheduledFor: now, FailedAt: now, LeaseExpiresAt: now.Add(-time.Hour)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.job.Claimable(now); got != tc.want {
				t.Errorf("Claimable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJobLeaseHeldBy is the question every write a worker makes asks. A worker
// whose lease expired and was claimed by somebody else must not be able to
// complete, fail, or extend the job it no longer holds.
func TestJobLeaseHeldBy(t *testing.T) {
	now := jobTime()
	live := Job{LeaseOwner: "w1", LeaseExpiresAt: now.Add(time.Minute)}

	if !live.LeaseHeldBy("w1", now) {
		t.Error("the holder does not hold its own live lease")
	}
	if live.LeaseHeldBy("w2", now) {
		t.Error("somebody else holds the lease")
	}
	if live.LeaseHeldBy("", now) {
		t.Error("a job with no worker name holds a lease")
	}
	if live.LeaseHeldBy("w1", now.Add(time.Hour)) {
		t.Error("an expired lease is still a lease")
	}
	// A job nobody holds is held by nobody, including by the empty name.
	if (Job{}).LeaseHeldBy("", now) {
		t.Error("the empty worker name holds an unleased job")
	}
}

// TestJobExhausted is the off-by-one the claim's increment makes easy to get
// wrong: attempts already includes the attempt that has just failed.
func TestJobExhausted(t *testing.T) {
	for _, tc := range []struct {
		attempts, max int
		want          bool
	}{
		{0, 5, false},
		{1, 5, false},
		{4, 5, false},
		{5, 5, true},
		{6, 5, true},
		{1, 1, true},
	} {
		job := Job{Attempts: tc.attempts, MaxAttempts: tc.max}
		if got := job.Exhausted(); got != tc.want {
			t.Errorf("Job{attempts: %d, max: %d}.Exhausted() = %v, want %v",
				tc.attempts, tc.max, got, tc.want)
		}
	}
}

// TestNewJobNormalize is that the defaults this package declares and the
// defaults the column declares are the same defaults.
func TestNewJobNormalize(t *testing.T) {
	now := jobTime()

	got := (NewJob{Kind: "  noop  "}).Normalize(now)
	if got.Kind != "noop" {
		t.Errorf("Kind = %q, want it trimmed", got.Kind)
	}
	if got.Priority != PriorityNormal {
		t.Errorf("Priority = %d, want %d", got.Priority, PriorityNormal)
	}
	if got.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", got.MaxAttempts, DefaultMaxAttempts)
	}
	if !got.ScheduledFor.Equal(now) {
		t.Errorf("ScheduledFor = %v, want now (%v)", got.ScheduledFor, now)
	}
	if got.Payload != "{}" {
		t.Errorf("Payload = %q, want the empty object", got.Payload)
	}

	// What the caller did say is left alone.
	kept := (NewJob{
		Kind: "noop", Priority: PriorityBulk, MaxAttempts: 2,
		ScheduledFor: now.Add(time.Hour), Payload: `{"a":1}`,
	}).Normalize(now)
	if kept.Priority != PriorityBulk || kept.MaxAttempts != 2 || kept.Payload != `{"a":1}` {
		t.Errorf("Normalize overwrote what the caller set: %+v", kept)
	}
	if !kept.ScheduledFor.Equal(now.Add(time.Hour)) {
		t.Errorf("ScheduledFor = %v, want the hour the caller asked for", kept.ScheduledFor)
	}
}

func TestNewJobValidate(t *testing.T) {
	now := jobTime()

	// Normalized says whether the case is asked of a job that has been
	// through Normalize, which is how every caller asks it. The two
	// unnormalized cases are the ones Normalize would have repaired: a zero
	// priority and a zero attempt budget are defaults on the way in, and
	// values Validate must refuse if they somehow survive.
	for name, tc := range map[string]struct {
		job        NewJob
		normalized bool
		wantErr    bool
	}{
		"ordinary":               {job: NewJob{Kind: "noop"}, normalized: true},
		"most urgent":            {job: NewJob{Kind: "noop", Priority: PriorityUrgent}, normalized: true},
		"bulk":                   {job: NewJob{Kind: "noop", Priority: PriorityBulk}, normalized: true},
		"no kind":                {job: NewJob{}, normalized: true, wantErr: true},
		"blank kind":             {job: NewJob{Kind: "   "}, normalized: true, wantErr: true},
		"priority too low":       {job: NewJob{Kind: "noop", Priority: 6}, normalized: true, wantErr: true},
		"payload not an object":  {job: NewJob{Kind: "noop", Payload: `["a"]`}, normalized: true, wantErr: true},
		"payload not json":       {job: NewJob{Kind: "noop", Payload: "{"}, normalized: true, wantErr: true},
		"priority off the scale": {job: NewJob{Kind: "noop", Priority: 0, MaxAttempts: 1}, wantErr: true},
		"no attempts allowed":    {job: NewJob{Kind: "noop", Priority: PriorityNormal}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			job := tc.job
			if tc.normalized {
				job = job.Normalize(now)
			}
			err := job.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate(%+v) = nil, want an error", job)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("Validate returned %v, want it to wrap ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate(%+v) = %v", job, err)
			}
		})
	}
}

// TestRetryDelay is the shape of the backoff rather than its constants: it
// grows, and it stops. Retrying at once is how a queue turns one outage into
// five failures in the same second and a job somebody has to find by hand.
func TestRetryDelay(t *testing.T) {
	if got := RetryDelay(1); got != RetryBaseDelay {
		t.Errorf("RetryDelay(1) = %s, want %s", got, RetryBaseDelay)
	}
	// A zero or negative attempt is the first attempt rather than no wait at
	// all: this is called with a counter, and a counter that is wrong should
	// not produce a hot loop.
	if got := RetryDelay(0); got != RetryBaseDelay {
		t.Errorf("RetryDelay(0) = %s, want %s", got, RetryBaseDelay)
	}

	prev := time.Duration(0)
	for attempt := 1; attempt <= 20; attempt++ {
		d := RetryDelay(attempt)
		if d < prev {
			t.Errorf("RetryDelay(%d) = %s, less than RetryDelay(%d) = %s", attempt, d, attempt-1, prev)
		}
		if d > RetryMaxDelay {
			t.Errorf("RetryDelay(%d) = %s, past the cap of %s", attempt, d, RetryMaxDelay)
		}
		if d <= 0 {
			t.Fatalf("RetryDelay(%d) = %s; a retry with no wait is a hot loop", attempt, d)
		}
		prev = d
	}
	if RetryDelay(20) != RetryMaxDelay {
		t.Errorf("RetryDelay(20) = %s, want the cap %s", RetryDelay(20), RetryMaxDelay)
	}
}

func TestJobFilter(t *testing.T) {
	t.Run("a contradiction is refused", func(t *testing.T) {
		err := (JobFilter{Pending: true, Failed: true}).Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("pending and failed together = %v, want invalid", err)
		}
	})
	t.Run("the limit is bounded", func(t *testing.T) {
		if err := (JobFilter{Limit: MaxJobListLimit}).Validate(); err != nil {
			t.Errorf("the maximum limit was refused: %v", err)
		}
		if err := (JobFilter{Limit: MaxJobListLimit + 1}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("a limit past the maximum = %v, want invalid", err)
		}
		if err := (JobFilter{Limit: -1}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("a negative limit = %v, want invalid", err)
		}
	})
	t.Run("normalize fills the limit in", func(t *testing.T) {
		if got := (JobFilter{}).Normalize().Limit; got != DefaultJobListLimit {
			t.Errorf("Limit = %d, want %d", got, DefaultJobListLimit)
		}
	})
	t.Run("describe names the question", func(t *testing.T) {
		for _, tc := range []struct {
			filter JobFilter
			want   string
		}{
			{JobFilter{}, "jobs"},
			{JobFilter{Pending: true}, "jobs pending"},
			{JobFilter{Failed: true}, "jobs failed"},
			{JobFilter{Failed: true, Kind: "noop"}, "jobs failed, of kind noop"},
		} {
			if got := tc.filter.Describe(); got != tc.want {
				t.Errorf("Describe(%+v) = %q, want %q", tc.filter, got, tc.want)
			}
		}
	})
}

func TestValidatePayload(t *testing.T) {
	for _, s := range []string{"", "   ", "{}", `{"document_version_id":7}`} {
		if err := ValidatePayload(s); err != nil {
			t.Errorf("ValidatePayload(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"[]", `"words"`, "7", "{", "not json"} {
		if err := ValidatePayload(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidatePayload(%q) = %v, want invalid", s, err)
		}
	}
	if got := NormalizePayload("  "); got != "{}" {
		t.Errorf("NormalizePayload(blank) = %q, want the empty object", got)
	}
}

// TestPriorityScale is that the constants are the scale the schema's CHECK
// declares, in the order the claim reads them.
func TestPriorityScale(t *testing.T) {
	if PriorityHighest != 1 || PriorityLowest != 5 {
		t.Fatalf("the priority scale is %d..%d, and the column CHECKs 1..5",
			PriorityHighest, PriorityLowest)
	}
	want := []int{PriorityUrgent, PriorityHigh, PriorityNormal, PriorityLow, PriorityBulk}
	for i, p := range want {
		if p != i+1 {
			t.Errorf("priority %d of the scale is %d", i+1, p)
		}
	}
	if PriorityNormal != 3 {
		t.Errorf("PriorityNormal = %d, and the column defaults to 3", PriorityNormal)
	}
}

// TestSubjectJob keeps the events subject kind a constant rather than a
// literal. A typo in one produces a history query that silently returns
// nothing.
func TestSubjectJob(t *testing.T) {
	if SubjectJob != "job" || strings.TrimSpace(SubjectJob) != SubjectJob {
		t.Errorf("SubjectJob = %q", SubjectJob)
	}
}
