// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the real binaries, because several of the M0 acceptance
// criteria are about the process rather than about a function: an exit code, a
// panic that must happen after main has begun, a response body that must reach
// the client before the process goes away.
//
// They are skipped under -short, since each one costs a compile.

const commands = "cmsd cmsdb earl"

// build compiles the three commands into dir and returns a lookup by name.
//
// Deliberately no tag: PLAN.md M0 acceptance 11 is that every command runs
// without one, and building the tagged variant here would test the wrong
// binary. The tagged half of the interlock is covered as a unit test in
// internal/buildenv, run in CI with -tags production.
func buildCommands(t *testing.T, dir string) map[string]string {
	t.Helper()

	bin := make(map[string]string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, name := range strings.Fields(commands) {
		out := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		wg.Go(func() {
			cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
			cmd.Dir = repoRoot(t)
			if b, err := cmd.CombinedOutput(); err != nil {
				mu.Lock()
				defer mu.Unlock()
				t.Errorf("go build ./cmd/%s: %v\n%s", name, err, b)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			bin[name] = out
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// The test runs in cmd/cmsd.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}
	return root
}

// run executes bin with env and args, returning stdout, stderr, and the exit
// code. An exit code of -1 means the process did not exit normally.
func run(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running %s %v: %v", bin, args, err)
		}
	}
	return out.String(), errb.String(), code
}

// runStdin is run with something on the command's standard input, which is
// how a password reaches "cmsdb bootstrap admin" and "earl login". A password
// is never a flag: arguments are visible in "ps" and land in shell history.
func runStdin(t *testing.T, bin, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runStdinEnv(t, bin, nil, stdin, args...)
}

func runStdinEnv(t *testing.T, bin string, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running %s %v: %v", bin, args, err)
		}
	}
	return out.String(), errb.String(), code
}

// initDB creates a database in a fresh temporary directory and returns the
// directory, which is what --db names (DESIGN.md 13.1).
//
// It runs the real "cmsdb init" rather than reaching into internal/store,
// because the point of these tests is the commands. t.TempDir already exists,
// which is the only reason this helper needs no mkdir: no test helper creates
// a directory in the database path, because a helper that creates what the
// commands refuse to create is a hole in invariant 19 wide enough for the
// production code.
func initDB(t *testing.T, cmsdb string) string {
	t.Helper()
	dir := t.TempDir()
	stdout, stderr, code := run(t, cmsdb, nil, "init", "--db", dir)
	if code != 0 {
		t.Fatalf("cmsdb init --db %s exited %d\nstdout: %s\nstderr: %s", dir, code, stdout, stderr)
	}
	return dir
}

// TestCommands covers the process-level M0 acceptance criteria. It builds
// once and runs everything against those binaries.
func TestCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	// Acceptance 2: each command prints a version and exits 0.
	t.Run("version", func(t *testing.T) {
		for name, path := range bin {
			stdout, stderr, code := run(t, path, nil, "version")
			if code != 0 {
				t.Errorf("%s version exited %d\nstderr: %s", name, code, stderr)
			}
			if !strings.HasPrefix(stdout, name+" ") {
				t.Errorf("%s version printed %q, want it to start with the command name", name, stdout)
			}
			if !strings.Contains(stdout, runtime.Version()) {
				t.Errorf("%s version printed %q, want the Go version in it", name, stdout)
			}
		}
	})

	// Acceptance 14, the untagged half, seen from outside: a binary built
	// without the tag must refuse to run with CMS_ENV=production, and must be
	// content with anything else.
	t.Run("interlock", func(t *testing.T) {
		for name, path := range bin {
			for _, tc := range []struct {
				env      []string
				wantExit bool
			}{
				{env: []string{"CMS_ENV="}, wantExit: false},
				{env: []string{"CMS_ENV=development"}, wantExit: false},
				{env: []string{"CMS_ENV=production"}, wantExit: true},
			} {
				_, stderr, code := run(t, path, tc.env, "version")
				failed := code != 0
				if failed != tc.wantExit {
					t.Errorf("%s version with %v exited %d, want failure=%v\nstderr: %s",
						name, tc.env, code, tc.wantExit, stderr)
					continue
				}
				if !tc.wantExit {
					continue
				}
				// Acceptance 15: the failure is reachable only after main has
				// begun. An init() would panic before cobra could ever run, and
				// the panic must be this package's, not something else's.
				if !strings.Contains(stderr, "buildenv: built without -tags production") {
					t.Errorf("%s panicked with %q, want the buildenv message", name, stderr)
				}
				if !strings.Contains(stderr, "main.main()") {
					t.Errorf("%s panicked outside main; Verify is called from main, never from init (invariant 18)\nstderr: %s",
						name, stderr)
				}
			}
		}
	})

	// Acceptance 8, at the process level and over a real socket. This one
	// gates release.
	t.Run("dev routes are 404 with the default environment", func(t *testing.T) {
		proc := start(t, bin["cmsd"], []string{"CMS_ENV="}, "serve", "--db", initDB(t, bin["cmsdb"]), "--addr", "127.0.0.1:0", "--timeout", "30s")

		for _, path := range []string{"/__development/shut-it-down", "/__development/log-me-in/a@b.c"} {
			resp, err := http.Get(proc.url + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404 with no --env and no CMS_ENV", path, resp.StatusCode)
			}
		}

		// And the banner says why, in a form an operator can grep for.
		if !strings.Contains(proc.stderr(), "environment=production") {
			t.Errorf("startup banner = %q, want environment=production", proc.stderr())
		}
	})

	// Acceptance 10: the response is fully received before the process exits,
	// and the process then exits 0. Assert on the body, not just the code.
	t.Run("dev shutdown flushes then exits 0", func(t *testing.T) {
		proc := start(t, bin["cmsd"], nil, "serve", "--db", initDB(t, bin["cmsdb"]), "--env", "development", "--addr", "127.0.0.1:0", "--timeout", "60s")

		resp, err := http.Get(proc.url + "/__development/shut-it-down")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("reading the body: %v; the response was not flushed before shutdown", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if got := strings.TrimSpace(string(body)); got != "shutting down" {
			t.Errorf("body = %q, want %q", got, "shutting down")
		}

		if code := proc.wait(t, 15*time.Second); code != 0 {
			t.Errorf("exit code = %d, want 0\nstderr: %s", code, proc.stderr())
		}
		// Acceptance 13, in the other environment.
		if !strings.Contains(proc.stderr(), "environment=development") {
			t.Errorf("startup banner = %q, want environment=development", proc.stderr())
		}
		if !strings.Contains(proc.stderr(), "*** ENVIRONMENT=development") {
			t.Errorf("startup banner = %q, want the loud warning", proc.stderr())
		}
	})

	// Acceptance 7, with a real duration: an expired timeout is a normal
	// shutdown, so the exit code is 0.
	t.Run("timeout exits 0", func(t *testing.T) {
		began := time.Now()
		proc := start(t, bin["cmsd"], nil, "serve", "--db", initDB(t, bin["cmsdb"]), "--addr", "127.0.0.1:0", "--timeout", "2s")
		code := proc.wait(t, 20*time.Second)
		elapsed := time.Since(began)

		if code != 0 {
			t.Errorf("exit code = %d, want 0\nstderr: %s", code, proc.stderr())
		}
		if elapsed < 2*time.Second {
			t.Errorf("exited after %v, before the 2s timeout could fire", elapsed)
		}
		if elapsed > 15*time.Second {
			t.Errorf("exited after %v, far past the 2s timeout", elapsed)
		}
		if !strings.Contains(proc.stderr(), "shutdown timer armed") {
			t.Errorf("stderr = %q, want one line naming the configured timeout", proc.stderr())
		}
	})
}

// TestGoRunWorksWithoutATag is PLAN.md M0 acceptance 11, spelled the way the
// criterion is: every command runs under "go run ./cmd/<name>" with no tag.
// There is one build tag in this repository and it must never become a
// prerequisite for running anything.
func TestGoRunWorksWithoutATag(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles three commands")
	}
	for _, name := range strings.Fields(commands) {
		cmd := exec.Command("go", "run", "./cmd/"+name, "version")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CMS_ENV=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("go run ./cmd/%s version: %v\n%s", name, err, out)
			continue
		}
		if !strings.HasPrefix(string(out), name+" ") {
			t.Errorf("go run ./cmd/%s version printed %q", name, out)
		}
	}
}

// process is a cmsd started by a test, with its listen address discovered from
// its own startup log rather than guessed.
type process struct {
	cmd  *exec.Cmd
	url  string
	errb *syncBuffer
	done chan error

	// The exit result is collected at most once and memoised here, so that
	// wait and stop can both be called. Reading cmd.ProcessState instead would
	// race with the goroutine running cmd.Wait.
	mu     sync.Mutex
	waited bool
	err    error
}

func start(t *testing.T, bin string, env []string, args ...string) *process {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}

	p := &process{cmd: cmd, errb: &syncBuffer{}, done: make(chan error, 1)}

	// Read the address out of the startup line. Guessing a free port and
	// hoping nobody takes it between the guess and the bind is how a test
	// becomes flaky once a year.
	addrs := make(chan string, 1)
	go func() {
		defer close(addrs)
		sc := bufio.NewScanner(stderr)
		// The startup line is key=value text in development and JSON in
		// production, so match both spellings of the same field rather than
		// forcing one log format on the test.
		re := regexp.MustCompile(`addr[=":\s]+(\d[\d.]*:\d+|\[[^\]]+\]:\d+)`)
		sent := false
		for sc.Scan() {
			line := sc.Text()
			p.errb.WriteString(line + "\n")
			if !sent {
				if m := re.FindStringSubmatch(line); m != nil {
					addrs <- m[1]
					sent = true
				}
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()

	select {
	case addr, ok := <-addrs:
		if !ok {
			t.Fatalf("cmsd exited before it logged a listen address\nstderr: %s", p.errb.String())
		}
		p.url = "http://" + addr
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("cmsd did not log a listen address\nstderr: %s", p.errb.String())
	}

	t.Cleanup(p.stop)
	return p
}

func (p *process) stderr() string { return p.errb.String() }

func (p *process) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	err, exited := p.reap(within)
	if !exited {
		_ = p.cmd.Process.Kill()
		t.Fatalf("cmsd did not exit within %s\nstderr: %s", within, p.errb.String())
	}
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for cmsd: %v", err)
	return -1
}

// reap collects the exit result at most once, memoising it.
func (p *process) reap(within time.Duration) (err error, exited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited {
		return p.err, true
	}
	select {
	case err := <-p.done:
		p.waited, p.err = true, err
		return err, true
	case <-time.After(within):
		return nil, false
	}
}

// stop kills the process if it is still running. It is the cleanup for every
// process a test starts, so that a failing assertion never leaves a cmsd
// holding a port.
func (p *process) stop() {
	p.mu.Lock()
	done := p.waited
	p.mu.Unlock()
	if done {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.reap(10 * time.Second)
}

// syncBuffer is a strings.Builder guarded for the reader goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) WriteString(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.WriteString(v)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
