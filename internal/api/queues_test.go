// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// The transport half of M5. What is under test here is the status code, the
// document shape, and that a filter which does not parse is refused rather
// than dropped; the refusals themselves are asserted in internal/service,
// where the rows written can be counted.

// listDocs runs GET /api/v1/documents with a query string and returns the uids.
func (h *harness) listDocs(t *testing.T, token, query string) []string {
	t.Helper()
	w := h.do(t, http.MethodGet, "/api/v1/documents?"+query, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/documents?%s = %d %s", query, w.Code, w.Body)
	}
	var out struct {
		Documents []documentResponse `json:"documents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	uids := make([]string, 0, len(out.Documents))
	for _, d := range out.Documents {
		uids = append(uids, d.UID)
	}
	return uids
}

// TestAssignmentRoundTrip drives the assignment and due-date subresources over
// HTTP.
func TestAssignmentRoundTrip(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	writer := h.user(t, "writer@example.com", domain.Edit)
	token := h.login(t, "editor@example.com")

	doc := h.createDoc(t, token, "Hand it over")
	base := "/api/v1/documents/" + doc.UID

	// POST the assignment, with a due date on the same request.
	w := h.do(t, http.MethodPost, base+"/assignment", token, map[string]any{
		"user": writer.UID, "due_at": "48h",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s/assignment = %d %s", base, w.Code, w.Body)
	}
	got := decodeDoc(t, w.Body.Bytes())
	if got.AssignedTo != writer.UID {
		t.Errorf("assigned_to = %q, want the writer's uid %q", got.AssignedTo, writer.UID)
	}
	if got.AssignedToName == "" {
		t.Error("assigned_to_name is empty; a person reads this column")
	}
	if got.DueAt == nil {
		t.Fatal("due_at is absent after a request that set one")
	}
	if want := h.svc.Now().Add(48 * time.Hour); !got.DueAt.Equal(want) {
		t.Errorf("due_at = %v, want %v: the duration is relative to the server's clock", got.DueAt, want)
	}
	if got.Overdue {
		t.Error("a deadline two days out is reported overdue")
	}

	// PUT the due date on its own; the assignee is untouched.
	w = h.do(t, http.MethodPut, base+"/due", token, map[string]any{"at": "2026-03-01"})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT %s/due = %d %s", base, w.Code, w.Body)
	}
	got = decodeDoc(t, w.Body.Bytes())
	if got.AssignedTo != writer.UID {
		t.Errorf("assigned_to = %q after moving only the due date", got.AssignedTo)
	}
	if got.DueAt == nil || got.DueAt.Format("2006-01-02") != "2026-03-01" {
		t.Errorf("due_at = %v, want 2026-03-01", got.DueAt)
	}
	// The fixture's clock is in February 2026, so a March deadline is not yet
	// past; the flag is computed against the server's clock, not the client's.
	if got.Overdue {
		t.Error("a deadline in March is overdue on a clock reading February")
	}

	// DELETE the due date; the assignee survives.
	w = h.do(t, http.MethodDelete, base+"/due", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE %s/due = %d %s", base, w.Code, w.Body)
	}
	got = decodeDoc(t, w.Body.Bytes())
	if got.DueAt != nil {
		t.Errorf("due_at = %v after clearing", got.DueAt)
	}
	if got.AssignedTo != writer.UID {
		t.Errorf("clearing the deadline unassigned the document")
	}

	// DELETE the assignment.
	w = h.do(t, http.MethodDelete, base+"/assignment", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE %s/assignment = %d %s", base, w.Code, w.Body)
	}
	if got = decodeDoc(t, w.Body.Bytes()); got.AssignedTo != "" {
		t.Errorf("assigned_to = %q after unassigning", got.AssignedTo)
	}
}

// TestAssignmentRefusals: each of the shapes a client gets wrong, and the
// status DESIGN.md 12's table gives it.
func TestAssignmentRefusals(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	h.user(t, "reader@example.com", domain.Read)
	token := h.login(t, "editor@example.com")
	readerToken := h.login(t, "reader@example.com")

	doc := h.createDoc(t, token, "Refusals")
	base := "/api/v1/documents/" + doc.UID

	t.Run("an unparseable due date is 422", func(t *testing.T) {
		w := h.do(t, http.MethodPost, base+"/assignment", token, map[string]any{
			"user": "me", "due_at": "next tuesday",
		})
		assertProblem(t, w, http.StatusUnprocessableEntity, "invalid")
	})

	t.Run("a user nobody is, is 404", func(t *testing.T) {
		w := h.do(t, http.MethodPost, base+"/assignment", token, map[string]any{
			"user": "01JQZZZZZZZZZZZZZZZZZZZZZZ",
		})
		assertProblem(t, w, http.StatusNotFound, "not-found")
	})

	t.Run("a reader may not assign", func(t *testing.T) {
		w := h.do(t, http.MethodPost, base+"/assignment", readerToken, map[string]any{"user": "me"})
		assertProblem(t, w, http.StatusForbidden, "forbidden")
	})

	t.Run("a document nobody may see is 404", func(t *testing.T) {
		w := h.do(t, http.MethodPost, "/api/v1/documents/nope/assignment", token, map[string]any{"user": "me"})
		assertProblem(t, w, http.StatusNotFound, "not-found")
	})

	t.Run("there is no route that sets a state", func(t *testing.T) {
		// PATCH is the draft editor, and it must not grow an assignee or a
		// state field. Sending one is a body with nothing it recognises.
		w := h.do(t, http.MethodPatch, base, token, map[string]any{"state": "published"})
		if w.Code == http.StatusOK {
			t.Errorf("PATCH with a state field returned 200: %s", w.Body)
		}
	})
}

// TestDocumentListFilters is PLAN.md M5 acceptance 1 over HTTP, and the
// refusal of a filter that does not parse.
func TestDocumentListFilters(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Publish)
	writer := h.user(t, "writer@example.com", domain.Edit)
	token := h.login(t, "editor@example.com")

	loose := h.createDoc(t, token, "Nobody's")
	taken := h.createDoc(t, token, "Taken")

	// A slug is needed for the submit transition's has_slug guard.
	for _, uid := range []string{loose.UID, taken.UID} {
		checkinWithSlug(t, h, token, uid)
		w := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/transitions", token,
			map[string]any{"to": "review"})
		if w.Code != http.StatusOK {
			t.Fatalf("submitting %s = %d %s", uid, w.Code, w.Body)
		}
	}
	w := h.do(t, http.MethodPost, "/api/v1/documents/"+taken.UID+"/assignment", token,
		map[string]any{"user": writer.UID})
	if w.Code != http.StatusOK {
		t.Fatalf("assigning = %d %s", w.Code, w.Body)
	}

	t.Run("state and unassigned", func(t *testing.T) {
		got := h.listDocs(t, token, "state=review&unassigned=true")
		if len(got) != 1 || got[0] != loose.UID {
			t.Errorf("got %v, want exactly %q", got, loose.UID)
		}
	})

	t.Run("a bare flag means yes", func(t *testing.T) {
		if got := h.listDocs(t, token, "unassigned"); len(got) != 1 || got[0] != loose.UID {
			t.Errorf("got %v, want exactly %q", got, loose.UID)
		}
	})

	t.Run("by assignee", func(t *testing.T) {
		got := h.listDocs(t, token, "assignee="+writer.UID)
		if len(got) != 1 || got[0] != taken.UID {
			t.Errorf("got %v, want exactly %q", got, taken.UID)
		}
	})

	t.Run("assignee=me is the caller", func(t *testing.T) {
		if got := h.listDocs(t, token, "assignee=me"); len(got) != 0 {
			t.Errorf("got %v, want nothing: the editor has nothing assigned", got)
		}
	})

	t.Run("by state", func(t *testing.T) {
		if got := h.listDocs(t, token, "state=review"); len(got) != 2 {
			t.Errorf("got %v, want both", got)
		}
		if got := h.listDocs(t, token, "state=draft"); len(got) != 0 {
			t.Errorf("got %v, want nothing", got)
		}
	})

	t.Run("by site", func(t *testing.T) {
		if got := h.listDocs(t, token, "site=1"); len(got) != 2 {
			t.Errorf("got %v, want both", got)
		}
		if got := h.listDocs(t, token, "site=2"); len(got) != 0 {
			t.Errorf("got %v, want nothing", got)
		}
	})

	// A filter that does not parse is refused rather than ignored. A filter
	// accepted and dropped is a list of the wrong documents presented as the
	// right ones.
	for _, query := range []string{
		"unassigned=perhaps",
		"overdue=sometimes",
		"limit=lots",
		"limit=-1",
		fmt.Sprintf("limit=%d", domain.MaxListLimit+1),
		"assignee=me&unassigned=true",
	} {
		t.Run("refuses "+query, func(t *testing.T) {
			w := h.do(t, http.MethodGet, "/api/v1/documents?"+query, token, nil)
			if w.Code == http.StatusOK {
				t.Errorf("GET ?%s returned 200; the filter was ignored: %s", query, w.Body)
			}
		})
	}
}

// TestQueues drives the saved queues over HTTP.
func TestQueues(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Publish)
	token := h.login(t, "editor@example.com")

	doc := h.createDoc(t, token, "Mine")
	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/assignment", token,
		map[string]any{"user": "me"})
	if w.Code != http.StatusOK {
		t.Fatalf("assigning = %d %s", w.Code, w.Body)
	}

	t.Run("the menu lists what the server serves", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/queues", token, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/queues = %d %s", w.Code, w.Body)
		}
		var out struct {
			Queues []queueResponse `json:"queues"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Queues) == 0 {
			t.Fatal("the server serves no queues")
		}
		// The question each one asks is in the response, so that a client can
		// explain an empty list without reading the server's configuration.
		for _, q := range out.Queues {
			if q.Slug == "" || q.Name == "" {
				t.Errorf("queue %+v has no slug or no name", q)
			}
		}
	})

	t.Run("mine resolves to the caller", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/queues/mine", token, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/queues/mine = %d %s", w.Code, w.Body)
		}
		var q queueResponse
		if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		if q.Count != 1 || len(q.Documents) != 1 || q.Documents[0].UID != doc.UID {
			t.Errorf("mine = %+v, want exactly %q", q, doc.UID)
		}
	})

	t.Run("a slug nobody defined is 404", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/queues/everything-important", token, nil)
		assertProblem(t, w, http.StatusNotFound, "not-found")
	})

	t.Run("a queue needs a credential", func(t *testing.T) {
		w := h.do(t, http.MethodGet, "/api/v1/queues/mine", "", nil)
		assertProblem(t, w, http.StatusUnauthorized, "unauthenticated")
	})
}

// checkinWithSlug gives a document a slug and closes the draft, which is what
// the submit transition's has_slug guard wants.
func checkinWithSlug(t *testing.T, h *harness, token, uid string) {
	t.Helper()
	base := "/api/v1/documents/" + uid
	if w := h.do(t, http.MethodPost, base+"/checkout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPatch, base, token, map[string]any{"slug": "a-slug"}); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPost, base+"/checkin", token, map[string]any{"note": "ready"}); w.Code != http.StatusOK {
		t.Fatalf("checkin = %d %s", w.Code, w.Body)
	}
}

func decodeDoc(t *testing.T, body []byte) documentResponse {
	t.Helper()
	var out documentResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding a document response: %v\n%s", err, body)
	}
	return out
}
