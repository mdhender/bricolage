// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
)

// The job routes (PLAN.md M6). The transport is what is under test, so
// everything below it is real.

// failedJob enqueues a job and drives it to abandoned, which is the state the
// retry route is defined on.
func (h *harness) failedJob(t *testing.T, kind string) domain.Job {
	t.Helper()
	job, err := h.svc.EnqueueJob(t.Context(), domain.NewJob{Kind: kind, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	// Claim until this job comes up rather than failing whatever the queue
	// happens to hand back first: a test that ran after one which left a job
	// pending would otherwise fail that one. Everything claimed on the way
	// keeps its lease, which takes it out of the pool without making it
	// claimable again.
	q := h.svc.JobQueue()
	for {
		claimed, ok, err := q.Claim(t.Context(), "worker-1")
		if err != nil || !ok {
			t.Fatalf("job %s was never claimed: %v, %v", job.UID, ok, err)
		}
		if claimed.UID != job.UID {
			continue
		}
		if err := q.Fail(t.Context(), claimed, "worker-1", errors.New("it broke")); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		break
	}

	failed, err := q.Job(t.Context(), job.UID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if failed.FailedAt.IsZero() {
		t.Fatalf("the job did not reach failed: %+v", failed)
	}
	return failed
}

type jobListBody struct {
	Jobs  []jobResponse `json:"jobs"`
	Count int           `json:"count"`
	Asks  string        `json:"asks"`
}

func decodeJobList(t *testing.T, body []byte) jobListBody {
	t.Helper()
	var out jobListBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the job list: %v\n%s", err, body)
	}
	return out
}

// TestListJobs is GET /api/v1/jobs and its filters.
func TestListJobs(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	failed := h.failedJob(t, "noop")
	pending, err := h.svc.EnqueueJob(t.Context(), domain.NewJob{Kind: "other", Priority: domain.PriorityUrgent})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	for name, tc := range map[string]struct {
		query string
		want  []string
	}{
		"everything": {"", []string{pending.UID, failed.UID}},
		"pending":    {"?pending", []string{pending.UID}},
		"failed":     {"?failed=true", []string{failed.UID}},
		"by kind":    {"?kind=other", []string{pending.UID}},
	} {
		t.Run(name, func(t *testing.T) {
			w := h.do(t, http.MethodGet, "/api/v1/jobs"+tc.query, token, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/jobs%s = %d %s", tc.query, w.Code, w.Body)
			}
			got := decodeJobList(t, w.Body.Bytes())
			if got.Count != len(tc.want) {
				t.Fatalf("count = %d, want %d: %+v", got.Count, len(tc.want), got.Jobs)
			}
			for i, uid := range tc.want {
				if got.Jobs[i].UID != uid {
					t.Errorf("job %d is %s, want %s", i, got.Jobs[i].UID, uid)
				}
			}
			if got.Asks == "" {
				t.Error("the response does not say which question it answered")
			}
		})
	}

	t.Run("a contradiction is 422", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/jobs?pending&failed", token, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("pending and failed together = %d, want 422: %s", w.Code, w.Body)
		}
	})

	t.Run("an unreadable flag is refused rather than read as no", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/jobs?pending=perhaps", token, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("?pending=perhaps = %d, want 422: %s", w.Code, w.Body)
		}
	})
}

// TestJobResponseHidesThePayload keeps the handler's argument out of the API.
// A payload names a version today and may name anything tomorrow; a queue
// listing is not the place to hand it back out.
func TestJobResponseHidesThePayload(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	if _, err := h.svc.EnqueueJob(t.Context(), domain.NewJob{
		Kind: "noop", Payload: `{"secret":"do not serve me"}`,
	}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	w := h.do(t, http.MethodGet, "/api/v1/jobs", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs = %d %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, "do not serve me") {
		t.Errorf("the listing served the job's payload:\n%s", body)
	}
}

// TestJobStatusIsDerivedAgainstTheServerClock is why the response carries a
// status rather than making the client work one out. An expired lease is not a
// lease, and a client that drew "running" from a lease_owner column would draw
// it over a job nobody is running.
func TestJobStatusIsDerivedAgainstTheServerClock(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	if _, err := h.svc.EnqueueJob(t.Context(), domain.NewJob{Kind: "noop"}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	q := h.svc.JobQueue()
	if _, ok, err := q.Claim(t.Context(), "worker-1"); err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}

	read := func(t *testing.T) jobResponse {
		t.Helper()
		w := h.do(t, http.MethodGet, "/api/v1/jobs", token, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/jobs = %d %s", w.Code, w.Body)
		}
		got := decodeJobList(t, w.Body.Bytes())
		if len(got.Jobs) != 1 {
			t.Fatalf("%d jobs, want 1", len(got.Jobs))
		}
		return got.Jobs[0]
	}

	running := read(t)
	if running.Status != string(domain.JobRunning) {
		t.Errorf("status = %q while the lease is live, want %q", running.Status, domain.JobRunning)
	}
	if running.Worker != "worker-1" {
		t.Errorf("worker = %q, want worker-1", running.Worker)
	}

	h.clock.Advance(2 * time.Hour)
	stale := read(t)
	if stale.Status != string(domain.JobPending) {
		t.Errorf("status = %q after the lease expired, want %q", stale.Status, domain.JobPending)
	}
	if stale.Worker != "" || stale.LeaseExpiresAt != nil {
		t.Errorf("an expired lease is still rendered as one: %+v", stale)
	}
}

// TestRetryJobRoute is POST /api/v1/jobs/{uid}/retry and the status codes
// DESIGN.md 12's table gives for each way it can be refused.
func TestRetryJobRoute(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	h.user(t, "reader@example.com", domain.Read)
	h.user(t, "nobody@example.com", domain.NoPrivilege)

	adminToken := h.login(t, "admin@example.com")
	readerToken := h.login(t, "reader@example.com")
	nobodyToken := h.login(t, "nobody@example.com")

	t.Run("unauthenticated is 401", func(t *testing.T) {
		job := h.failedJob(t, "noop")
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+job.UID+"/retry", "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("= %d, want 401: %s", w.Code, w.Body)
		}
	})

	t.Run("no grant at all is 404", func(t *testing.T) {
		job := h.failedJob(t, "noop")
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+job.UID+"/retry", nobodyToken, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404: %s", w.Code, w.Body)
		}
	})

	t.Run("read but not publish is 403", func(t *testing.T) {
		job := h.failedJob(t, "noop")
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+job.UID+"/retry", readerToken, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("= %d, want 403: %s", w.Code, w.Body)
		}
	})

	t.Run("an unknown uid is 404", func(t *testing.T) {
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+ids.MustNew(start)+"/retry", adminToken, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404: %s", w.Code, w.Body)
		}
	})

	t.Run("a job that has not failed is 409", func(t *testing.T) {
		job, err := h.svc.EnqueueJob(t.Context(), domain.NewJob{Kind: "noop"})
		if err != nil {
			t.Fatalf("EnqueueJob: %v", err)
		}
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+job.UID+"/retry", adminToken, nil)
		if w.Code != http.StatusConflict {
			t.Errorf("= %d, want 409: %s", w.Code, w.Body)
		}
	})

	t.Run("publish retries it and gets the job back", func(t *testing.T) {
		job := h.failedJob(t, "noop")
		w := h.do(t, http.MethodPost, "/api/v1/jobs/"+job.UID+"/retry", adminToken, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("= %d, want 200: %s", w.Code, w.Body)
		}
		var got jobResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding: %v\n%s", err, w.Body)
		}
		if got.UID != job.UID {
			t.Errorf("uid = %s, want %s", got.UID, job.UID)
		}
		if got.Status != string(domain.JobPending) {
			t.Errorf("status = %q after a retry, want %q", got.Status, domain.JobPending)
		}
		if got.Attempts != 0 || got.FailedAt != nil {
			t.Errorf("the retried job is still failed: %+v", got)
		}
		if got.LastError == "" {
			t.Error("the retry cleared last_error; how it failed is worth keeping")
		}
	})
}

// TestJobRoutesSpeakUID is invariant 10 for this milestone. DESIGN.md 12
// spelled the retry route as /jobs/{id}/retry; an invariant outranks a path
// spelled in an example, and the route was corrected rather than the rule.
func TestJobRoutesSpeakUID(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	job := h.failedJob(t, "noop")
	if job.ID == 0 {
		t.Fatal("the fixture produced a job with no primary key")
	}

	// The integer primary key names nothing.
	w := h.do(t, http.MethodPost, "/api/v1/jobs/1/retry", token, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("the integer primary key reached the retry route: %d %s", w.Code, w.Body)
	}

	// And it does not appear in what a client is served.
	w = h.do(t, http.MethodGet, "/api/v1/jobs", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs = %d %s", w.Code, w.Body)
	}
	var envelope struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	raw := envelope.Jobs
	if len(raw) == 0 {
		t.Fatal("the listing is empty")
	}
	for _, field := range []string{"id", "job_id", "created_by"} {
		if _, ok := raw[0][field]; ok {
			t.Errorf("the job response carries %q, which is an internal key (invariant 10)", field)
		}
	}
	if raw[0]["uid"] != job.UID {
		t.Errorf("uid = %v, want %s", raw[0]["uid"], job.UID)
	}
}
