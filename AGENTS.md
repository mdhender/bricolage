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
internal/{clock,ids,config}
schema/                 .sql migrations, embedded
deploy/                 reverse-proxy config; Caddyfile.dev
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

```sh
caddy run --config deploy/Caddyfile.dev     # https://htmx-app.localhost:8443
go build -tags dev -o bin/cmsd ./cmd/cmsd   # dev routes need the tag
bin/cmsd serve --db ./dev.db --addr 127.0.0.1:18443 --env development --timeout 60m
earl login --server https://htmx-app.localhost:8443 --dev --email admin@example.com
```

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

The first two require **both** a binary built with `-tags dev` **and**
`--env development`. The environment defaults to `production`, so neither the
build nor the configuration alone is enough. Anything other than the exact
string `development` — unset, empty, `dev`, `Development` — is `production`.

A default build returns 404 for both, by design. If you get a 404, you either
built without the tag or did not set the environment, and that is the guard
working rather than a bug to route around. Fix the build or the flag; never
weaken the guard.

`log-me-in` logs in as an **existing** user; it does not create accounts. Run
`cmsdb bootstrap admin` first.

## Development workflow

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

- Format changed Go files with `gofmt`.
- Add or update focused tests when changing behaviour.
- Do not edit `go.sum` by hand; use Go module commands.
- Verify library APIs with `go doc` against the installed toolchain rather than
  assuming.

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
   needs to move a document, it calls the engine.
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
9. **Migrations are append-only.** Once committed and released, a migration file
   is never edited. Fix mistakes with a new migration.
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
16. **`/__development/*` routes require `//go:build dev` *and*
    `environment == "development"`.** The environment defaults to `production`
    and is never inferred from a hostname, a listen address, or a TTY. Never
    register these routes from ordinary code, never add a second switch that
    turns them on, and never relax the guard to make a test pass. A default
    build must return 404 for every one of them, and CI asserts it. Either
    route is a complete authentication bypass if it reaches production.
17. **Graceful shutdown is one code path**, reached by `SIGTERM`, `--timeout`,
    and the dev shutdown route alike. Do not write a second one.

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
  applied through the same code path `cmsdb` uses. Not a mock.
- `service` tests assert on emitted events as well as returned values. An
  operation that does not write its event is not finished.
- Golden files for rendering and for `earl --json`; regenerate with `-update`.
- Every milestone gets one end-to-end test driving `earl` against a `cmsd` on a
  temporary database.
- Concurrency tests run under `-race`.

## Pull requests

- One milestone per branch where practical; one coherent change otherwise.
- The PR description states which milestone and which acceptance criteria it
  satisfies, and names any it does not.
- Assign every issue and pull request to the repository owner at creation:
  `gh pr create --assignee @me ...`, `gh issue create --assignee @me ...`.
  If the account lacks permission to set assignees, create it without and say
  so rather than treating it as a failure.
- Do not commit or push unless asked.

## When you are stuck

State the ambiguity, pick the option most consistent with `docs/DESIGN.md`,
implement it, and flag the assumption in the PR description. Do not stall on a
question you can answer by reading the design document, and do not silently
choose differently from what it says — if the design is wrong, change the
design in the same PR and explain why.
