# Project Guidance

## Start here

1. `docs/DESIGN.md` — what we are building and why. The reference for every
   implementation decision.
2. `docs/PLAN.md` — thirteen milestones, in order, with acceptance criteria.
3. This file — the rules you follow while doing it.

Read the design section relevant to your task before writing code. Do not work
from memory of a previous session; the documents are the memory.

## Project context

This repository is a **greenfield Go content management system** focused on
editorial workflow and publishing. The module path stays
`github.com/mdhender/bricolage` for the duration of the build; a rename may
happen once the work is finished. Do not rename it, and do not introduce a
second name for the project in code or documentation. It is **not** a port of the Perl Bricolage
CMS and is not schema-, API-, or wire-compatible with it.

The Perl implementation at
[github.com/bricoleurs/bricolage](https://github.com/bricoleurs/bricolage) is a
**source of requirements and cautionary tales**, not a specification. When
`docs/DESIGN.md` cites its behaviour, that is evidence about the problem domain.
Never translate its code. Never reproduce its schema. Its last official release
was 2.0.1; the 2.1.0 tree was unfinished work toward a 2.2.0 that never shipped.

Deliberately not supported: SOAP (clients use the REST API — there are none yet
to break), migration from an existing Bricolage database, and every mod_perl-era
concept listed in `docs/DESIGN.md` §1.

## Commands

Three binaries, one module:

| Command | Purpose |
|---|---|
| `cmsdb` | initialise, migrate, bootstrap, seed, and check the database |
| `cmsd`  | serve the REST API, the HTMX UI, and background job workers |
| `earl`  | exercise the API from the command line |

`earl` is a first-class client and the acceptance-test harness for every
milestone, not a debug toy. If `earl` cannot do it, the API is incomplete.

## Repository layout

```
cmd/{cmsdb,cmsd,earl}   command entry points; flags and wiring only
internal/domain         types, invariants, pure functions — imports nothing local
internal/store          all SQL; converts rows to domain types
internal/migrate        embedded migrations
internal/service        use cases; owns transactions
internal/{workflow,authz,publish,jobs,events,render}   subsystem logic
internal/{api,web}      transports
internal/web/templates  the UI's html/template files; internal/web/static its CSS and HTMX
internal/web/devroutes  the /__development/* handlers; no build tag gates them
internal/edge           what both transports agree on: the status mapping, the session cookie
internal/server         composition root: the route table, the one shutdown path
internal/{clock,ids,config,buildenv}
internal/reqctx         per-request context values: client address, request id, identity
internal/migrate/schema .sql migrations, embedded via go:embed
deploy/                 reverse-proxy notes; Caddyfile.dev is an EXAMPLE ONLY
testdata/               fixtures
```

Dependencies point **downward only**: transport → service → domain, with store
below service. See `docs/DESIGN.md` §3. Keep behaviour in the package that owns
it; never put application logic in `cmd/`.

New code goes in `internal/`, not `pkg/`. Nothing here is meant to be imported
by another module.

## Toolchain and dependencies

**Go 1.25 is a hard floor.** `net/http.CrossOriginProtection` arrived in it and
is the CSRF defence for the HTMX UI. Do not lower the floor.

- **Routing is `net/http.ServeMux` only.** `pkg/way` is deleted and nothing
  replaces it; method and wildcard patterns have covered our needs since
  Go 1.22. Do not add a router dependency.
- **`cobra` is retained** for the three commands.
- Prefer the standard library. A new third-party dependency needs a reason in
  the PR description.

## Running it locally

`cmsd` speaks plain HTTP on loopback and **never terminates TLS.** Caddy
terminates it, in development as in production, so there is one serving model
rather than two.

### Caddy is a service. Do not touch it.

**Caddy runs as a machine-wide Homebrew service and you must never run it
yourself.** Do not run `caddy run`, `caddy start`, `caddy reload`, or
`caddy trust`. In particular, **never run
`caddy run --config deploy/Caddyfile.dev`** — that file is an **EXAMPLE ONLY**,
kept as documentation of the proxy shape, and is not the configuration this
machine serves.

The reason is certificates. The service runs with
`HOME=/opt/homebrew/var/lib`, so its internal CA lives in
`/opt/homebrew/var/lib/caddy/pki/`. Running `caddy` as your own user account
uses `~/Library/Application Support/Caddy/pki/` instead, which mints a *second*
CA root carrying the same subject name and trusts it. Two same-subject anchors
make certificate chain building non-deterministic: HTTPS to `*.localhost:8443`
fails at random, in Chrome first, and clearing it needs `sudo` keychain
surgery. We have already lost an afternoon to exactly this.

The service reads `/opt/homebrew/etc/Caddyfile`, which already terminates TLS
for `https://htmx-app.localhost:8443` and proxies it to `127.0.0.1:18443`.
There is nothing for you to configure. You start `cmsd` and nothing else:

```sh
brew services list | grep caddy    # expect "started" — do not start it yourself
mkdir -p var                       # you create the directory; no command ever will
go run ./cmd/cmsdb init --db ./var                 # creates ./var/cms.db
go run ./cmd/cmsdb seed --db ./var                 # roles and their grants, one site
go run ./cmd/cmsdb bootstrap admin --db ./var --email admin@example.com --name Admin
go run ./cmd/cmsd serve --db ./var --addr 127.0.0.1:18443 --env development --timeout 60m \
    --templates ./templates --preview ./var/preview --output ./var/output   # all three optional
go run ./cmd/earl login --server https://htmx-app.localhost:8443 --dev --email admin@example.com
go run ./cmd/earl whoami
```

The HTML UI is the same server at the same origin: point a browser at
`https://htmx-app.localhost:8443/` and sign in, or visit
`/__development/log-me-in/admin@example.com?returnTo=/` and skip the password.
Everything it does, `earl` does too — that is `docs/PLAN.md` M13 acceptance 5,
and a test asserts it.

`seed` before `bootstrap admin`: the admin role is seeded, and bootstrap
assigns it. Run them the other way round and bootstrap says so rather than
silently creating a user with no role.

`bootstrap admin` prints a generated password **once**, or reads one from stdin
with `--password-stdin`. It never takes a password as a flag — arguments are
visible in `ps` and land in shell history.

**Rendering and publishing need three directories you create yourself.**
`cmsd serve` takes an optional `--templates DIR`, `--preview DIR`, and
`--output DIR`; without them everything else works and `earl doc preview` and
`earl doc publish` answer 503 naming the flag that was not given. None of the
three roots is ever created (invariant 19). The template tree is
`<templates>/<site domain>/<category path>/<element type key>.gohtml`, so for
the seeded site the fallback template is
`templates/assemblage.localhost/story.gohtml` — `assemblage.localhost` being
what `cmsdb seed` writes unless `--site-domain` says otherwise. It is the site's
domain and not the public origin: the two are the same length of string and
different questions, and on a real installation they are different hosts. The output tree is where published
files land, at the URI the output channel builds; the directories *inside* it
are made by `internal/publish` and are the one exception invariant 19 carries.
Full rules: `docs/DESIGN.md` §8.3 and §8.4.

**`--db` names a directory, not a file** (`DESIGN.md` §13.1). The database
inside it is always `cms.db`. Nothing in this system creates a directory, so
`mkdir` is your job and a missing directory is a hard failure rather than a
silently created empty CMS. `cmsd` never creates a database and never migrates
one: if it will not start, run `cmsdb migrate up` or fix the path — do not reach
for a flag that makes the server do it.

**`go run ./cmd/...` is how we run commands locally.** There is no build step
and no tag to remember: the one build tag we have, `production`, is for release
binaries only and never for local work. Build a binary when you want one; never
make it a prerequisite for running the thing.

If Caddy is not started, ask a human to start it (`brew services start caddy`).
Do not start it yourself, and do not work around it by having `cmsd` serve TLS.

When diagnosing local TLS, check the chain actually served on the wire and test
with `/usr/bin/curl`. Homebrew's `openssl` and `curl` carry their own CA
bundles and will disagree with the macOS keychain.

Point clients at the public origin, not at `127.0.0.1:18443`. Talking to the
listener directly bypasses the proxy and exercises a path that does not exist in
production. See `deploy/README.md` and `docs/DESIGN.md` §11.

### Working without a human

Three affordances exist so you can drive the system unattended. Full rules in
`docs/DESIGN.md` §11, "Development affordances".

| Need | Use |
|---|---|
| Log in without typing a password | `GET /__development/log-me-in/{email}?returnTo={url}`, or `earl login --dev --email E` |
| Stop a server you started | `curl .../__development/shut-it-down` |
| Not leave a server running after you finish | `cmsd serve --timeout 60m` |

**Always pass `--timeout`.** An orphaned `cmsd` holds a SQLite lock and the next
session fails in a way that looks like a code bug. A generous timeout costs
nothing; forgetting one costs somebody an hour.

The first two require `--env development` (or `CMS_ENV=development`) and
nothing else — **no build tag gates them.** The routes are simply not
registered in any other environment. The environment defaults to `production`, and
anything other than the exact string `development` — unset, empty, `dev`,
`Development` — is `production`.

If you get a 404, you did not set the environment. That is the guard working
rather than a bug to route around: fix the flag, and never weaken the guard or
register the routes unconditionally "just for a minute".

`log-me-in` logs in as an **existing** user; it does not create accounts. Run
`cmsdb bootstrap admin` first.

## Development workflow

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
grep -rn 'os\.MkdirAll\|os\.Mkdir(' ./cmd ./internal    # must print nothing
```

`make lint` runs that grep and the two that keep invariants 16 and 4. Run
`make check` rather than remembering the list.

A migration that needs foreign key enforcement off while it runs says so with
`-- migrate: disable-foreign-keys` on a line of its own; `internal/migrate`
reads the directive. Exactly one migration has it, and only SQLite's documented
twelve-step `ALTER` procedure justifies it — see
`internal/migrate/schema/0005_workflow.sql`.

- Run commands with `go run ./cmd/<name>`. Do not make a `go build` step a
  prerequisite for running anything, and do not add a build tag — `production`
  is the only one, it is described below, and it gates no code.
- Format changed Go files with `gofmt`.
- Add or update focused tests when changing behaviour.
- Do not edit `go.sum` by hand; use Go module commands.
- Verify library APIs with `go doc` against the installed toolchain rather than
  assuming.

### Building for the server

You will rarely need this; it is here so that nobody invents a different recipe.
Release binaries are cross-compiled with `-tags production` and rsynced:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -tags production -trimpath -o deploy/linux/amd64/cmsd ./cmd/cmsd
rsync -av --chmod=F755 deploy/linux/amd64/ deploy@SERVER:/opt/cms/bin/
```

The tag adds one thing — `buildenv.Verify()`, which panics unless `CMS_ENV` is
exported as exactly `production`. Binaries built without it panic if `CMS_ENV`
*is* `production`. It gates no routes and no features. Full rules in
`deploy/README.md` and `docs/DESIGN.md` §14.

**Never deploy anything yourself, and never build a production binary as a side
effect of some other task.** If a change needs shipping, say so and let a human
run it.

## Invariants

These are not style preferences. Violating one is a bug even if the tests pass.

1. **`internal/domain` performs no I/O** and imports nothing else in this
   repository. No `database/sql`, no `net/http`, no `time.Now()`.
2. **All SQL lives in `internal/store`.** No SQL string appears anywhere else,
   ever.
3. **`time.Now()` appears only in `main` and `internal/clock`.** Everything else
   takes a `Clock`. This is what makes leases, schedules, and due dates
   testable.
4. **`internal/workflow` is the only writer of `documents.state`.** If something
   needs to move a document, it calls the engine. This does not bend
   invariant 2: the one statement that writes the column lives in
   `internal/store/workflow.go`, inside `ApplyTransition`, which takes the
   engine's `check` as a callback and cannot run without it, and which has
   exactly one caller. `make lint` and CI grep for a second statement and for a
   second caller (`DESIGN.md` §6.3).
5. **`Available` and `Do` share one `check` function.** The UI must be rendered
   from the same code that enforces the rule. This is the structural fix for the
   worst defect in the system we learned from, where the permission check lived
   only in the template that drew the menu.
6. **Never merge a configurable guard name without its enforcement, in the same
   commit.** The Perl original shipped `pre_chk_rules` and `post_chk_rules` with
   tables, foreign keys, indexes, an API, and no implementation anywhere. A rule
   column that nothing reads is worse than no column, because it lies to whoever
   reads the schema.
7. **Every state-changing operation writes an event** carrying enough payload to
   reconstruct what happened.
8. **Publish jobs pin a version id, never a document id.** The approved version
   is what ships, whatever the draft has become.
9. **Migrations are append-only.** Once committed, a migration file is never
   edited, never reordered, and never removed; fix mistakes with a new
   migration. The number of files only grows. There is **no beta exception** —
   there used to be one, permitting a squash, and it is withdrawn
   (`DESIGN.md` §13.6). Squashing lowers the migration count while every
   deployed database keeps the old one, leaving those databases permanently
   *ahead* of the binary: refused by invariant 21, unreachable by
   `cmsdb migrate up`, and unrepairable without doing the one thing invariant 21
   forbids. **Never write `PRAGMA user_version`.** It belongs to
   `sqlitemigration`, its value is the number of migrations applied, and the
   only thing that may move it is applying one.
10. **The API speaks `uid` only.** Internal integer primary keys never appear in
    a URL, a JSON body, or user-facing output.
11. **Detect constraint violations by result code**, never by matching error
    text.
12. **Nobody may grant a privilege they do not hold** over that scope.
13. **Session cookies are always `Secure`.** Never gate the flag on
    `r.TLS != nil`; behind the proxy that expression is always false, and the
    resulting code ships insecure cookies while looking careful.
14. **Forwarded headers are trusted only from configured proxy addresses.**
    `X-Forwarded-*` is attacker-controlled otherwise. Resolve the client
    address once, in middleware, and read it from the context.
15. **`cmsd` binds loopback and never terminates TLS.** No `:8080`, no
    `0.0.0.0`, no certificate handling, no HSTS header of its own.
16. **`/__development/*` routes are registered only when
    `environment == "development"`.** They are left out of the mux entirely, not
    matched and then refused, so the route table always tells the truth. No
    build tag is involved — the resolved environment is the whole gate, which is
    why nothing may erode it. The `production` tag (invariant 18) is a placement
    check, not a feature switch; never use it to hide a route. The environment
    defaults to `production` and is never inferred from a hostname, a listen
    address, or a TTY. Never register these routes
    from ordinary code, never add a second switch that turns them on, and never
    relax the guard to make a test pass. With the default environment every one
    of them must 404, and CI asserts it. Either route is a complete
    authentication bypass if it reaches production.
17. **Graceful shutdown is one code path**, reached by `SIGTERM`, `--timeout`,
    and the dev shutdown route alike. Do not write a second one.
18. **`buildenv.Verify()` is called from `main`, never from `init()`.** `main`
    decides when the check runs, because it may want to handle `version` or
    `--help` first. Under `-tags production` it panics unless `CMS_ENV` is
    exported as exactly `production`; without the tag it panics if `CMS_ENV`
    *is* `production`. It reads the raw environment variable, not the resolved
    `environment`, and it must never grow a second responsibility.
19. **Nothing creates a directory it was told to use.** `--db`, `--templates`,
    `--preview`, and `--output` all name directories that must already exist,
    and the database inside `--db` is always the constant `cms.db`. A missing
    one is a hard failure naming the directory, at startup rather than at first
    use. A tool that creates what it cannot find turns a typo into a
    plausible-looking, empty system. `os.Mkdir` and `os.MkdirAll` appear
    nowhere in the database path — not in `cmsdb`, not in `cmsd`, not in a test
    helper, not in a convenience wrapper.

    **The one exception is the interior of the output tree**
    (`internal/publish/tree.go`, M9). `/features/film/2026/03/01/` is not
    configuration: it is computed from a category path, a URI format, and a
    cover date, there is no typo it could be, and the alternative is an output
    tree that is not a tree — a publishing system whose output no web server
    can serve. The property invariant 19 buys is kept in full, because the root
    is still never created. Two things keep the exception narrow: every path is
    validated by `domain.OutputPath` first, and every write goes through an
    `os.Root` opened on the output directory, so a category named `../../etc`
    cannot address a byte outside the tree even if the first check were wrong.
    `make lint` allows the call in that one file and nowhere else — including
    through an `os.Root` method, because reaching for `root.MkdirAll` to slip
    past a grep for `os.MkdirAll` is exactly the convenience wrapper this rule
    names.
20. **Only `cmsdb init` creates a database, and only `cmsdb` migrates one.**
    `cmsd` does neither, ever, under any flag. Open flags are always written out
    explicitly, because the zero value of `sqlitex.PoolOptions.Flags` and the
    no-flag form of `sqlite.OpenConn` both include `OpenCreate`. Only the create
    path names `OpenCreate`.
21. **`cmsd` verifies the database it opened and refuses to start otherwise.**
    `PRAGMA application_id` must be `0x434D5330` (the ASCII bytes of `CMS0`) and
    `PRAGMA user_version` must equal the number of embedded migrations, exactly —
    ahead and behind are both hard failures. Both pragmas are maintained by
    `sqlitemigration`; **never write `PRAGMA user_version`**, never write a
    `schema_migrations` table, and never keep any other hand-rolled version
    bookkeeping. The value means "this many migrations have been applied", and
    applying one is the only thing permitted to move it — which is what makes
    it trustworthy, and what invariant 9 protects by refusing a squash.
    `sqlitemigration`'s own application-ID check adopts an ID of `0` when the
    database has no schema, so `cmsd` performs this check itself rather than
    inheriting that leniency (`DESIGN.md` §13.4).
22. **`foreign_keys = ON` on every connection of every store**, in-memory
    included; **WAL on every persistent store**. Both are per-connection
    settings, so one connection that skips them is silently wrong for its whole
    life.
23. **Autocomplete is denied by default on every form control.** A field opts
    *in* to being filled, with a comment saying why; a field that declares
    nothing is a bug, because "nothing" means the browser guesses. Deny with
    `{{noAutofill}}`, which emits `autocomplete="off"` and the three vendor
    opt-outs together; a test walks the embedded templates and fails on a
    control carrying neither that nor a deliberate token.

    This is more aggressive than the platform intends and the reason is not
    tidiness. Password managers treat `autocomplete="off"` as advisory, because
    sites have used it to break them on purpose, and Chrome overrides it wherever
    its heuristics feel confident. The signature they key on is `type="email"`
    with `name="email"` — which is exactly the shape of the *invite* box, a
    field whose one impossible value is the address of the person typing. So the
    address inputs that are not credentials are `type="text"` with
    `inputmode="email"`: the mobile keyboard keeps its `@` and the heuristic
    loses its cue. **Validation was never the client's job here** —
    `domain.ValidateEmail` is the check, and it is deliberately weak because the
    strong check is delivery.

    Exactly two forms opt in today, and both are the viewer's own credentials:
    the login form and the invitation redemption form. Anything else asking to
    opt in should explain, in the template, whose data the field holds — if the
    answer is not "the person looking at the screen", the answer is no.

## Code conventions

- Idiomatic Go. Follow existing package naming and functional-option patterns.
- Keep changes small. Do not introduce an abstraction until it has a clear
  package-level responsibility or a second real use.
- Propagate errors to the layer that can handle them. Sentinel errors in
  `domain`, wrapped with `%w`, inspected with `errors.Is`. Map to HTTP status
  in exactly one place at the transport edge.
- Reserve process exits and fatal logging for the CLI boundary.
- `context.Context` is the first parameter of every service, store, and job
  method, and cancellation is honoured.
- Structured logging with `log/slog`. Never log tokens, password hashes, or
  document bodies.
- Every Go source file carries the project copyright and MIT licence header.
  Preserve it when editing; include it in new files.
- `pkg/way/` is deleted. If any fragment is kept for any reason, its attribution
  and licensing are kept with it.

## Testing

- `domain` is pure; test it exhaustively and table-driven.
- `store` is tested against a real in-memory SQLite database with all migrations
  applied through the same code path `cmsdb` uses. Not a mock. Foreign keys are
  on there too (invariant 22); a test that passes with them off proves nothing.
- `service` tests assert on emitted events as well as returned values. An
  operation that does not write its event is not finished.
- Golden files for rendering and for `earl --json`; regenerate with `-update`.
- Every milestone gets one end-to-end test driving `earl` against a `cmsd` on a
  temporary database: `t.TempDir()`, then `cmsdb init` against it. **No test
  helper calls `os.MkdirAll`** — a helper that creates what the commands refuse
  to create is a hole in invariant 19 wide enough for the production code.
- Concurrency tests run under `-race`.

## Committing

**Committing and pushing to `main` is authorised for the duration of the beta.**
You do not need to ask first. What decides where a commit goes is whether there
is an issue:

- **No issue: commit straight to `main`.** Do not open a branch for it.
- **Working an issue: always work on a branch**, and reference the issue in
  every commit message on it — `Refs #1` while the work is in progress,
  `Closes #1` on the commit that finishes it. Close the issue when the work is
  complete: a merged `Closes` does it, and otherwise close it explicitly with
  `gh issue close`. An issue left open after its work has shipped is a lie in
  the same way a rule column nothing reads is (invariant 6).

### The version bump

**Every commit that changes code bumps the version in `version.go`, in that
same commit.** Documentation-only changes do not bump — a version that moves
when nothing executable changed tells you nothing about what is running.

The bump is semantic: **patch for a fix, minor for a feature.** `PreRelease`
stays `beta` until the project moves to release.

Keeping the bump in the commit that earns it is the point. A separate "bump
version" commit means the version in any given tree is the version of some
earlier tree, and `cmsd version` — the thing an operator reads off a running
server — stops naming the code that is running.

## Pull requests

- One milestone per branch where practical; one coherent change otherwise.
- The PR description states which milestone and which acceptance criteria it
  satisfies, and names any it does not.
- Assign every issue and pull request to the repository owner at creation:
  `gh pr create --assignee @me ...`, `gh issue create --assignee @me ...`.
  If the account lacks permission to set assignees, create it without and say
  so rather than treating it as a failure.

## When you are stuck

State the ambiguity, pick the option most consistent with `docs/DESIGN.md`,
implement it, and flag the assumption in the PR description. Do not stall on a
question you can answer by reading the design document, and do not silently
choose differently from what it says — if the design is wrong, change the
design in the same PR and explain why.
