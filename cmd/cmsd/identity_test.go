// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The M2 acceptance criteria that are about the processes: what "cmsdb
// bootstrap admin" prints and refuses, what the development login route
// answers in each environment, and the "earl login then earl whoami" round
// trip that is this milestone's end-to-end test (DESIGN.md 15).
//
// They live in cmd/cmsd because the harness that builds and runs the three
// binaries already does.

// bootstrapped returns a database directory with the schema, the seed, and one
// administrator, and the password that administrator has.
//
// t.TempDir already exists, which is the only reason this needs no mkdir: no
// test helper creates a directory in the database path, because a helper that
// creates what the commands refuse to create is a hole in invariant 19 wide
// enough for the production code.
func bootstrapped(t *testing.T, cmsdb, email, password string) string {
	t.Helper()
	dir := initDB(t, cmsdb)

	if _, stderr, code := run(t, cmsdb, nil, "seed", "--db", dir); code != 0 {
		t.Fatalf("cmsdb seed exited %d\nstderr: %s", code, stderr)
	}
	stdout, stderr, code := runStdin(t, cmsdb, password,
		"bootstrap", "admin", "--db", dir, "--email", email, "--name", "Admin", "--password-stdin")
	if code != 0 {
		t.Fatalf("cmsdb bootstrap admin exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return dir
}

// TestBootstrapAdmin is PLAN.md M2 acceptance 1 and 2.
func TestBootstrapAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]

	// Acceptance 1, first half: a generated password is printed exactly once.
	t.Run("a generated password is printed once", func(t *testing.T) {
		dir := initDB(t, cmsdb)
		stdout, stderr, code := run(t, cmsdb,
			nil, "bootstrap", "admin", "--db", dir, "--email", "admin@example.com", "--name", "Admin")
		if code != 0 {
			t.Fatalf("exited %d\nstderr: %s", code, stderr)
		}

		password := passwordFrom(t, stdout)
		if len(password) < 20 {
			t.Errorf("the generated password is %d characters: %q", len(password), password)
		}
		if strings.Count(stdout, password) != 1 {
			t.Errorf("the password appears %d times in the output; it is printed once", strings.Count(stdout, password))
		}
		if strings.Contains(stderr, password) {
			t.Error("the password was written to stderr as well as stdout")
		}

		// Printed once means printed once: nothing stores it, so nothing can
		// show it again.
		stdout2, _, _ := run(t, cmsdb, nil, "migrate", "status", "--db", dir)
		if strings.Contains(stdout2, password) {
			t.Error("a later command printed the password again")
		}

		// And it is the password: it logs in.
		if _, stderr, code := runStdin(t, cmsdb, password,
			"bootstrap", "admin", "--db", dir, "--email", "second@example.com", "--name", "Second", "--password-stdin"); code != 0 {
			t.Fatalf("the generated password was not accepted as a password: %s", stderr)
		}
	})

	// Acceptance 1, second half: a second run with the same email exits
	// non-zero and changes nothing.
	t.Run("a second run with the same email changes nothing", func(t *testing.T) {
		dir := initDB(t, cmsdb)
		if _, stderr, code := run(t, cmsdb,
			nil, "bootstrap", "admin", "--db", dir, "--email", "admin@example.com", "--name", "Admin"); code != 0 {
			t.Fatalf("the first run exited %d\nstderr: %s", code, stderr)
		}

		stdout, stderr, code := run(t, cmsdb,
			nil, "bootstrap", "admin", "--db", dir, "--email", "admin@example.com", "--name", "Someone Else")
		if code == 0 {
			t.Fatalf("the second run exited 0\nstdout: %s", stdout)
		}
		if !strings.Contains(stderr, "already exists") {
			t.Errorf("stderr = %q, want it to say the user already exists", stderr)
		}
		if strings.Contains(stdout, "password:") {
			t.Errorf("the second run printed a password: %q", stdout)
		}

		// Case folding is part of "the same email": users.email is UNIQUE, and
		// without folding "Admin@example.com" is a second account.
		if _, _, code := run(t, cmsdb,
			nil, "bootstrap", "admin", "--db", dir, "--email", "ADMIN@example.com", "--name", "Shouty"); code == 0 {
			t.Error("the same address in another case created a second account")
		}
	})

	// Acceptance 2: a password supplied on a command-line flag is rejected
	// outright. Arguments are visible in "ps" to every other account on the
	// machine and they land in shell history.
	t.Run("a password on a flag is rejected", func(t *testing.T) {
		dir := initDB(t, cmsdb)

		for _, flag := range []string{"--password", "--pass", "--pw"} {
			args := []string{"bootstrap", "admin", "--db", dir,
				"--email", "admin@example.com", "--name", "Admin", flag, "hunter2hunter2"}
			stdout, stderr, code := run(t, cmsdb, nil, args...)
			if code == 0 {
				t.Errorf("cmsdb bootstrap admin %s exited 0\nstdout: %s", flag, stdout)
			}
			if !strings.Contains(stderr, "unknown flag") {
				t.Errorf("%s was refused with %q, want \"unknown flag\"", flag, stderr)
			}
		}

		// And the help does not advertise one, so nobody goes looking.
		stdout, stderr, _ := run(t, cmsdb, nil, "bootstrap", "admin", "--help")
		help := stdout + stderr
		for _, forbidden := range []string{"--password ", "--password="} {
			if strings.Contains(help, forbidden) {
				t.Errorf("the help mentions %q", forbidden)
			}
		}
		if !strings.Contains(help, "--password-stdin") {
			t.Error("the help does not mention --password-stdin, which is how a password is supplied")
		}
	})
}

// TestSeedIsIdempotent: "cmsdb seed" runs more than once in a developer's life
// and must not duplicate or fail.
func TestSeedIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	dir := initDB(t, bin["cmsdb"])

	first, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir)
	if code != 0 {
		t.Fatalf("seed exited %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(first, "role: admin") || !strings.Contains(first, "site:") {
		t.Errorf("seed printed %q", first)
	}

	second, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir)
	if code != 0 {
		t.Fatalf("the second seed exited %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(second, "already present") {
		t.Errorf("the second seed printed %q, want it to report what was already there", second)
	}

	// And the database is still sound: no duplicate rows, no dangling keys.
	if _, stderr, code := run(t, bin["cmsdb"], nil, "check", "--db", dir); code != 0 {
		t.Fatalf("cmsdb check after two seeds exited %d\nstderr: %s", code, stderr)
	}
}

// TestDevLoginRoute is PLAN.md M2 acceptance 9 and 11, over a real socket.
//
// The first case gates release: with the default environment the route is not
// registered, so it 404s because nothing answers it rather than because
// something refused.
func TestDevLoginRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const email = "admin@example.com"
	dir := bootstrapped(t, bin["cmsdb"], email, "correct horse battery")

	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want int
	}{
		{
			name: "the default environment",
			env:  []string{"CMS_ENV="},
			want: http.StatusNotFound,
		},
		{
			name: "--env production",
			env:  []string{"CMS_ENV="},
			args: []string{"--env", "production"},
			want: http.StatusNotFound,
		},
		{
			name: "--env development",
			args: []string{"--env", "development"},
			want: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"serve", "--db", dir, "--addr", "127.0.0.1:0", "--timeout", "60s"}, tc.args...)
			proc := start(t, bin["cmsd"], tc.env, args...)

			body, status := get(t, proc.url+"/__development/log-me-in/"+email, "application/json")
			if status != tc.want {
				t.Fatalf("status = %d, want %d\nbody: %s", status, tc.want, body)
			}
			if tc.want != http.StatusOK {
				return
			}

			var login struct {
				Token string `json:"token"`
				User  struct {
					Email string `json:"email"`
				} `json:"user"`
			}
			if err := json.Unmarshal([]byte(body), &login); err != nil {
				t.Fatalf("the body is not JSON: %v\n%s", err, body)
			}
			if login.Token == "" || login.User.Email != email {
				t.Fatalf("body = %s", body)
			}

			// Acceptance 11: an unknown email is a 404 and creates no
			// account. The account count is checked by trying to log the
			// unknown address in a second time -- a route that created
			// accounts would answer 200 on the retry.
			for range 2 {
				body, status := get(t, proc.url+"/__development/log-me-in/nobody@example.com", "application/json")
				if status != http.StatusNotFound {
					t.Fatalf("an unknown email returned %d\nbody: %s", status, body)
				}
			}

			// The session it issued is an ordinary one and works on the API.
			me, status := getWithToken(t, proc.url+"/api/v1/me", login.Token)
			if status != http.StatusOK {
				t.Fatalf("GET /api/v1/me with the development token = %d\n%s", status, me)
			}
			if !strings.Contains(me, email) {
				t.Errorf("/api/v1/me = %s", me)
			}
		})
	}
}

// TestEarlLoginAndWhoami is PLAN.md M2 acceptance 3 and the milestone's
// end-to-end test: earl driven against a cmsd on a temporary database.
//
// It runs against the listener rather than through the Caddy proxy the
// criterion names. The proxy hop is what production has, and it is exercised by
// hand and by TestEarlOverTheProxy below when the service is running; making
// every run of "go test" depend on a machine-wide Homebrew service would make
// the suite unrunnable in CI, which has no Caddy and no *.localhost
// certificate.
func TestEarlLoginAndWhoami(t *testing.T) {
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

	// The credentials file is redirected, so a test never writes to the
	// person running it. The directory is t.TempDir, which already exists:
	// earl creates no directory either (invariant 19).
	creds := filepath.Join(t.TempDir(), "credentials.json")
	env := []string{"EARL_CREDENTIALS=" + creds, "EARL_SERVER=" + proc.url}

	t.Run("password login then whoami", func(t *testing.T) {
		stdout, stderr, code := runStdinEnv(t, bin["earl"], env, password,
			"login", "--email", email, "--password-stdin")
		if code != 0 {
			t.Fatalf("earl login exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, email) {
			t.Errorf("earl login printed %q", stdout)
		}

		// The token is at mode 0600. A token readable by every account on the
		// machine is not a stored credential, it is a published one.
		info, err := os.Stat(creds)
		if err != nil {
			t.Fatalf("the credentials file was not written: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("credentials mode = %#o, want 0600", perm)
		}

		stdout, stderr, code = run(t, bin["earl"], env, "whoami")
		if code != 0 {
			t.Fatalf("earl whoami exited %d\nstderr: %s", code, stderr)
		}
		for _, want := range []string{email, "Admin", "admin (Administrator)", "publish over everything"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("earl whoami printed %q, want it to contain %q", stdout, want)
			}
		}
	})

	// Acceptance 10, end to end: the session the development route issues
	// carries exactly what the password session carries. The comparison is
	// between two /api/v1/me documents, which is where the roles and grants
	// are.
	t.Run("the development login grants no elevation", func(t *testing.T) {
		if _, stderr, code := runStdinEnv(t, bin["earl"], env, password,
			"login", "--email", email, "--password-stdin"); code != 0 {
			t.Fatalf("earl login: %s", stderr)
		}
		byPassword, stderr, code := run(t, bin["earl"], env, "whoami", "--json")
		if code != 0 {
			t.Fatalf("earl whoami: %s", stderr)
		}

		if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
			t.Fatalf("earl login --dev: %s", stderr)
		}
		byDev, stderr, code := run(t, bin["earl"], env, "whoami", "--json")
		if code != 0 {
			t.Fatalf("earl whoami: %s", stderr)
		}

		// The expiry differs, because the two sessions were issued at
		// different instants; everything about who the caller is must not.
		if got, want := withoutExpiry(t, byDev), withoutExpiry(t, byPassword); got != want {
			t.Errorf("the development session carries\n%s\nand the password session carries\n%s", got, want)
		}
	})

	t.Run("logout ends the session", func(t *testing.T) {
		if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
			t.Fatalf("earl login --dev: %s", stderr)
		}
		if _, stderr, code := run(t, bin["earl"], env, "logout"); code != 0 {
			t.Fatalf("earl logout: %s", stderr)
		}
		if _, _, code := run(t, bin["earl"], env, "whoami"); code == 0 {
			t.Error("earl whoami succeeded after logout")
		}
	})

	// The administration commands, which is what "if earl cannot do it, the
	// API is incomplete" means for the grants half of this milestone.
	//
	// The refusal half -- an actor may not confer what they do not hold -- is
	// asserted where the answer is precise: in internal/service, where the
	// rows written can be counted, and in internal/api, where the status code
	// is. Here the administrator holds publish globally, so there is nothing
	// they cannot confer and nothing to refuse.
	t.Run("admin grant and assign", func(t *testing.T) {
		if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
			t.Fatalf("earl login --dev: %s", stderr)
		}

		stdout, stderr, code := run(t, bin["earl"], env,
			"admin", "grant", "--role", "writer", "--privilege", "edit", "--doc-kind", "story")
		if code != 0 {
			t.Fatalf("earl admin grant exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "edit") || !strings.Contains(stdout, "kind=story") {
			t.Errorf("earl admin grant printed %q", stdout)
		}

		// The uid comes from whoami, because the API speaks uid and never the
		// integer key (invariant 10).
		me, stderr, code := run(t, bin["earl"], env, "whoami", "--json")
		if code != 0 {
			t.Fatalf("earl whoami: %s", stderr)
		}
		var doc struct {
			User struct {
				UID string `json:"uid"`
			} `json:"user"`
		}
		if err := json.Unmarshal([]byte(me), &doc); err != nil {
			t.Fatal(err)
		}

		if _, stderr, code := run(t, bin["earl"], env,
			"admin", "assign", "--user", doc.User.UID, "--role", "writer"); code != 0 {
			t.Fatalf("earl admin assign exited %d\nstderr: %s", code, stderr)
		}
		stdout, stderr, code = run(t, bin["earl"], env, "whoami")
		if code != 0 {
			t.Fatalf("earl whoami: %s", stderr)
		}
		if !strings.Contains(stdout, "writer") {
			t.Errorf("earl whoami does not list the assigned role:\n%s", stdout)
		}
	})
}

// TestEarlOverTheProxy is PLAN.md M2 acceptance 3 as written: over the Caddy
// proxy rather than direct to the listener.
//
// It is skipped unless the machine-wide Homebrew Caddy service is running and
// already proxying the development origin to 127.0.0.1:18443, because that is
// a property of the machine and not of this repository. Never start Caddy to
// make this run: running it as your own account mints a second CA root with
// the same subject name and breaks HTTPS to *.localhost at random
// (AGENTS.md, "Caddy is a service").
func TestEarlOverTheProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	const origin = "https://htmx-app.localhost:8443"

	resp, err := http.Get(origin + "/healthz")
	if err != nil {
		t.Skipf("the Caddy service is not proxying %s: %v", origin, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	bin := buildCommands(t, t.TempDir())
	const email = "admin@example.com"
	dir := bootstrapped(t, bin["cmsdb"], email, "correct horse battery")

	// The proxy forwards to a fixed address, so this one binds it rather than
	// asking for a free port.
	proc := start(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:18443", "--timeout", "120s")
	_ = proc

	creds := filepath.Join(t.TempDir(), "credentials.json")
	env := []string{"EARL_CREDENTIALS=" + creds}

	if _, stderr, code := run(t, bin["earl"], env,
		"login", "--dev", "--email", email, "--server", origin); code != 0 {
		t.Fatalf("earl login over the proxy exited %d\nstderr: %s", code, stderr)
	}
	stdout, stderr, code := run(t, bin["earl"], env, "whoami")
	if code != 0 {
		t.Fatalf("earl whoami over the proxy exited %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, email) || !strings.Contains(stdout, origin) {
		t.Errorf("earl whoami printed %q", stdout)
	}
}

// passwordFrom pulls the generated password out of what bootstrap printed.
func passwordFrom(t *testing.T, stdout string) string {
	t.Helper()
	for line := range strings.SplitSeq(stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "password: "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("no password in the output:\n%s", stdout)
	return ""
}

// withoutExpiry re-renders a whoami document with the session expiry removed,
// so that two sessions issued at different instants can be compared on
// everything else.
func withoutExpiry(t *testing.T, body string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("whoami --json is not JSON: %v\n%s", err, body)
	}
	delete(doc, "session_expires_at")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// get performs a GET and returns the body and the status.
func get(t *testing.T, url, accept string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return send(t, req)
}

func getWithToken(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return send(t, req)
}

func send(t *testing.T, req *http.Request) (string, int) {
	t.Helper()
	// Redirects are not followed: the development route answers 302 when
	// returnTo is given, and following it would discard the response.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(body), resp.StatusCode
}

// TestSeedSiteDomain is issue #3 from the other end: a server can be brought up
// whose site domain matches the origin it actually serves, without hand-written
// SQL.
func TestSeedSiteDomain(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	t.Run("the domain given is the domain written", func(t *testing.T) {
		dir := initDB(t, bin["cmsdb"])
		stdout, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir,
			"--site-domain", "WWW.Example.COM")
		if code != 0 {
			t.Fatalf("seed exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "site: www.example.com") {
			t.Errorf("seed printed %q, want the folded domain", stdout)
		}
		if strings.Contains(stdout, "assemblage.localhost") {
			t.Errorf("seed wrote the default domain even though one was given: %q", stdout)
		}
	})

	t.Run("a name that could not be a host is refused before anything is written", func(t *testing.T) {
		dir := initDB(t, bin["cmsdb"])
		_, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir, "--site-domain", "not a host")
		if code == 0 {
			t.Fatal("seed accepted a domain that is not a host")
		}
		if !strings.Contains(stderr, "not a host") && !strings.Contains(stderr, "site domain") {
			t.Errorf("the refusal says %q; it has to name the value", stderr)
		}
		// Nothing was written, so the next seed is a first seed.
		stdout, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir)
		if code != 0 {
			t.Fatalf("seed after a refused one exited %d\nstderr: %s", code, stderr)
		}
		if strings.Contains(stdout, "already present") {
			t.Errorf("the refused seed left rows behind: %q", stdout)
		}
	})

	t.Run("seeding again with another domain does not create a second site", func(t *testing.T) {
		dir := initDB(t, bin["cmsdb"])
		if _, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir); code != 0 {
			t.Fatalf("seed exited %d\nstderr: %s", code, stderr)
		}
		_, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--db", dir, "--site-domain", "www.example.com")
		if code == 0 {
			t.Fatal("a second seed with another domain created a second site")
		}
		if !strings.Contains(stderr, "earl site update") {
			t.Errorf("the refusal says %q; it has to name the command that does what was asked", stderr)
		}
		if _, stderr, code := run(t, bin["cmsdb"], nil, "check", "--db", dir); code != 0 {
			t.Fatalf("cmsdb check after the refusal exited %d\nstderr: %s", code, stderr)
		}
	})
}
