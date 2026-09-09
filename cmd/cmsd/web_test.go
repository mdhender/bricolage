// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// M13's end-to-end test: the HTML UI driven against a cmsd on a temporary
// database, beside earl (AGENTS.md, "Testing").
//
// The two clients are used together on purpose. PLAN.md M13 acceptance 5 is
// that the UI performs no operation earl cannot, and the way to make that
// claim good at this level is to write with one and read with the other: a
// document created through a form is a document "earl doc show" prints, and a
// transition earl refuses is a transition the form cannot force.
//
// The cookie is carried by hand rather than by a net/http/cookiejar. The
// session cookie is Secure, unconditionally and correctly (invariant 13), and
// a jar will not send a Secure cookie over the plain-HTTP loopback hop this
// test talks to -- cmsd never terminates TLS (invariant 15), the proxy does.
// A browser reaching the public origin sends it; a test reaching the listener
// attaches it itself.

// browser is a client that keeps one session cookie.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
	cookie *http.Cookie
}

func newBrowser(t *testing.T, base string) *browser {
	t.Helper()
	return &browser{
		t:    t,
		base: base,
		client: &http.Client{
			// Redirects are followed by hand, because what a redirect says is
			// half of what these assertions are about.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// do performs one request, keeping any session cookie the answer sets.
func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "cms_session") && c.Value != "" {
			b.cookie = c
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("reading %s %s: %v", req.Method, req.URL, err)
	}
	return resp, string(body)
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodGet, b.base+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.do(req)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// What a browser sends when a form on this origin is submitted. The CSRF
	// protection reads it, and a request that did not send it would be
	// exercising the non-browser path instead.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", b.base)
	return b.do(req)
}

// TestWebUI covers PLAN.md M13 acceptances 2, 3, and 5 through the running
// server.
func TestWebUI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)
	proc := start(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s")
	env := earlEnv(t, proc.url)

	if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
		t.Fatalf("earl login --dev: %s", stderr)
	}
	earl := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, code := run(t, bin["earl"], env, args...)
		if code != 0 {
			t.Fatalf("earl %s exited %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
		}
		return stdout
	}

	b := newBrowser(t, proc.url)

	t.Run("a signed-out browser is sent to the form", func(t *testing.T) {
		resp, _ := b.get("/documents")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET /documents signed out = %d, want 303", resp.StatusCode)
		}
		if to := resp.Header.Get("Location"); !strings.HasPrefix(to, "/login") {
			t.Errorf("it redirected to %q, want the login form", to)
		}
	})

	t.Run("the form signs in", func(t *testing.T) {
		resp, _ := b.post("/login", url.Values{"email": {email}, "password": {password}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /login = %d, want 303", resp.StatusCode)
		}
		if b.cookie == nil {
			t.Fatal("signing in set no session cookie")
		}
		if !b.cookie.Secure || !b.cookie.HttpOnly {
			t.Errorf("the session cookie is %+v; it is always Secure and HttpOnly (invariant 13)", b.cookie)
		}
	})

	var uid string
	t.Run("a document created through the form is one earl can read", func(t *testing.T) {
		resp, body := b.post("/documents", url.Values{
			"site": {"1"}, "kind": {"story"}, "element_type": {"story"},
			"title":      {"The Quick Brown Fox"},
			"slug":       {"the-quick-brown-fox"},
			"cover_date": {"2026-03-01"},
			"field.body": {"The quick brown fox jumps over the lazy dog."},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /documents = %d, want 303: %s", resp.StatusCode, body)
		}
		uid = strings.TrimPrefix(strings.SplitN(resp.Header.Get("Location"), "?", 2)[0], "/documents/")
		if uid == "" {
			t.Fatalf("POST /documents redirected to %q, which names no document", resp.Header.Get("Location"))
		}

		var shown struct {
			UID     string `json:"uid"`
			Version struct {
				Title string `json:"title"`
			} `json:"version"`
		}
		out := earl(t, "doc", "show", "--json", uid)
		if err := json.Unmarshal([]byte(out), &shown); err != nil {
			t.Fatalf("earl doc show --json: %v\n%s", err, out)
		}
		if shown.Version.Title != "The Quick Brown Fox" {
			t.Errorf("earl reads the title as %q", shown.Version.Title)
		}
	})

	t.Run("the action bar is what earl reports", func(t *testing.T) {
		var listed struct {
			Transitions []struct {
				To        string `json:"to"`
				Name      string `json:"name"`
				Permitted bool   `json:"permitted"`
				Reason    string `json:"reason"`
			} `json:"transitions"`
		}
		out := earl(t, "doc", "transitions", "--json", uid)
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			t.Fatalf("earl doc transitions --json: %v\n%s", err, out)
		}
		if len(listed.Transitions) == 0 {
			t.Fatal("earl reports no transitions out of draft")
		}

		_, page := b.get("/documents/" + uid)
		for _, tr := range listed.Transitions {
			if !strings.Contains(page, tr.Name) {
				t.Errorf("the action bar omits %q, which earl lists", tr.Name)
			}
			if !tr.Permitted && tr.Reason != "" && !strings.Contains(page, tr.Reason) {
				t.Errorf("%q is refused (%q) and the page does not say why", tr.Name, tr.Reason)
			}
		}
	})

	t.Run("a forged transition is refused", func(t *testing.T) {
		// "published" is not declared from "draft" by the seeded process, so
		// the engine refuses it inside the transaction whoever asks.
		resp, _ := b.post("/documents/"+uid+"/transitions", url.Values{"to": {"published"}})
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("a forged POST = %d, want 409", resp.StatusCode)
		}

		var shown struct {
			State string `json:"state"`
		}
		out := earl(t, "doc", "show", "--json", uid)
		if err := json.Unmarshal([]byte(out), &shown); err != nil {
			t.Fatalf("earl doc show --json: %v\n%s", err, out)
		}
		if shown.State != "draft" {
			t.Errorf("earl reads the state as %q after a refused transition, want draft", shown.State)
		}
	})

	t.Run("a cross-origin write is refused", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			proc.url+"/documents/"+uid+"/transitions", strings.NewReader("to=review"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://evil.example.com")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, _ := b.do(req)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a cross-origin POST = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("the development login drives the UI", func(t *testing.T) {
		// The affordance that lets an agent work without a human at a
		// keyboard (DESIGN.md 11) issues an ordinary session, so it writes an
		// ordinary cookie and the UI accepts it like any other.
		fresh := newBrowser(t, proc.url)
		resp, _ := fresh.get("/__development/log-me-in/" + url.PathEscape(email) + "?returnTo=/")
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("log-me-in with a returnTo = %d, want 302", resp.StatusCode)
		}
		if fresh.cookie == nil {
			t.Fatal("log-me-in set no session cookie")
		}
		resp, page := fresh.get("/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / after log-me-in = %d", resp.StatusCode)
		}
		if !strings.Contains(page, "Dashboard") {
			t.Errorf("the dashboard did not render:\n%s", page)
		}
	})
}
