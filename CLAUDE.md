# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Read these first

`AGENTS.md` is the working contract for this repository: the 22 numbered
invariants, the code conventions, the testing rules, and the Caddy warnings.
Read it in full — this file is a summary and does not replace it.

- `docs/DESIGN.md` (1581 lines) — what is being built and why; the reference for
  every implementation decision. Read the section relevant to the task before
  writing code.
- `docs/PLAN.md` (660 lines) — milestones M0–M13, in order, each with acceptance
  criteria. Work is organised by milestone; a PR names the milestone and the
  criteria it satisfies.
- `AGENTS.md` — the rules while doing it.

Comments and commit messages in this repository cite `DESIGN.md §N`,
`PLAN.md M<n> acceptance <k>`, and `invariant <n>`. Follow those citations
rather than reasoning from the code alone; the documents are the memory.

## Commands

`make check` is the gate — `fmt vet lint build test race tagged` — and CI runs
the same steps spelled out individually so a failure names the rule it broke.

```sh
make check                 # run before opening a PR
make dev                   # go run ./cmd/cmsd serve --env development --timeout 60m
make help                  # list targets
go test ./internal/config/                   # one package
go test -run TestResolve ./internal/config/  # one test
go test -tags production ./internal/buildenv/   # the tagged half of the interlock
```

`make lint` is two greps, not a linter binary: nothing may call `os.Mkdir`/
`os.MkdirAll` (invariant 19), and only `internal/web/devroutes` may register a
`/__development/` pattern, with exactly one caller of `devroutes.Register`
(invariant 16). CI adds a third: `time.Now()` appears only in `main` and
`internal/clock` (invariant 3).

`make release` cross-compiles for linux/amd64 with `-tags production`. Never run
it as a side effect of another task, and never deploy.

### Running locally

Run commands with `go run ./cmd/<name>`. There is no build step and no tag to
remember; `production` is the only build tag and it is for release binaries.

`cmsd` speaks plain HTTP on `127.0.0.1:18443` and never terminates TLS. **Caddy
is a machine-wide Homebrew service — never run `caddy` yourself**, in particular
never `caddy run --config deploy/Caddyfile.dev` (that file is an example only).
Running it as your user account mints a second CA root with the same subject
name and breaks HTTPS to `*.localhost:8443` at random. If it is not started, ask
a human for `brew services start caddy`. Details and the reasoning:
`AGENTS.md`, `deploy/README.md`.

Always pass `--timeout` to `cmsd serve`. An orphaned server holds a SQLite lock
and the next session fails looking like a code bug.

`--db` names a *directory* that must already exist; the database inside it is
always `cms.db`, and `mkdir` is the human's job because nothing in this system
creates a directory.

## Architecture

Three commands, one module: `cmsdb` (database lifecycle), `cmsd` (REST API,
HTMX UI, job workers), `earl` (API client and the acceptance-test harness for
every milestone — if `earl` cannot do it, the API is incomplete).

Dependencies point downward only:

```
  internal/api (JSON)   internal/web (HTML/HTMX)   ← transports, no business logic
                 \        /
              internal/service                     ← use cases; owns transactions
                     |
              internal/domain                      ← pure; imports nothing local
                     |
              internal/store                       ← all SQL, zombiezen SQLite
```

`workflow`, `authz`, `publish`, `jobs`, `events`, `render` sit beside `service`.
`cmd/` is flags and wiring only. `internal/server` is the composition root and
holds the two things with nowhere else to live: the route table and the single
graceful-shutdown path.

Two structural ideas carry most of the design's weight:

- **The route table is built, not described.** `buildRoutes` registers handlers
  and records patterns in the same call, so `cmsd routes` prints what `serve`
  would actually mount. The one `if s.env.IsDevelopment()` in
  `internal/server/routes.go:62` is the entire gate on the development
  affordances — they are absent from the mux, not matched and refused.
- **The environment is the only switch.** `config.Resolve` (`--env`, then
  `$CMS_ENV`, then config file, then default `production`) treats the highest
  source that supplies *any* value as the winner, even a misspelled one, and
  recognises `development` only as that exact string. There is deliberately no
  `--dev` flag. Never infer the environment from a hostname, listen address, or
  TTY, and never add a second switch that turns the dev routes on.

The `production` build tag gates no routes and no features. It flips
`buildenv.Verify()`, called from `main` and never `init()`: tagged binaries
panic unless `CMS_ENV=production`, untagged binaries panic if it *is*.

## State of the tree

M0 is complete and M1 has not started: `cmsd` serves `/healthz` and the
development shutdown route, opens no database, and has no `--db` flag. Most
packages under `internal/` are a `doc.go` stating the package's responsibility
and permitted imports — read that doc before adding the first real file to one.

`pkg/way` is deleted and nothing replaces it; routing is `net/http.ServeMux`
patterns. Go 1.25 is a hard floor (`net/http.CrossOriginProtection` is the CSRF
defence for the HTMX UI). Prefer the standard library; a new dependency needs a
reason in the PR description.

## The rules most easily broken

The full list is `AGENTS.md`, "Invariants". These are the ones that ordinary
work walks into:

- Nothing creates a directory — not `cmsdb`, not `cmsd`, not a test helper.
- All SQL lives in `internal/store`. `internal/domain` does no I/O and imports
  nothing local.
- `time.Now()` only in `main` and `internal/clock`; everything else takes a
  `Clock`.
- One shutdown path, reached by SIGTERM, `--timeout`, and the dev route alike.
- Migrations are append-only *after beta*; the beta exception is a deliberate,
  separately announced squash, not a licence to edit one during ordinary work.
- Every Go file carries the copyright and MIT header.

## Commit workflow

Full rules and their reasoning: `AGENTS.md`, "Committing".

- **Committing and pushing to `main` is authorised** for the duration of the
  beta. No need to ask first.
- **No issue: commit straight to `main`.** Do not open a branch for it.
- **Working an issue: always work on a branch**, and reference the issue in
  every commit message — `Refs #1` in progress, `Closes #1` on the commit that
  finishes it. Close the issue when the work is complete.
- **Every commit that changes code bumps `version.go`**, in that same commit:
  patch for a fix, minor for a feature, `PreRelease` stays `beta` until the
  project moves to release. Documentation-only changes do not bump.
- Assign issues and pull requests at creation with `--assignee @me`.
