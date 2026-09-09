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

`make lint` is greps, not a linter binary: nothing may call `os.Mkdir`/
`os.MkdirAll` (invariant 19); only `internal/web/devroutes` may register a
`/__development/` pattern, with exactly one caller of `devroutes.Register`
(invariant 16); and `documents.state` is written by one statement, in
`internal/store/workflow.go`, whose `ApplyTransition` has one caller, in
`internal/workflow` (invariants 2 and 4). CI adds a fourth: `time.Now()`
appears only in `main` and `internal/clock` (invariant 3).

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

M0 through M5 are complete and M6 has not started. `cmsdb` can `init`,
`migrate status`, `migrate up [--to N]`, `bootstrap admin`, `seed [--demo]`,
`check`, and `vacuum`; `cmsd serve` requires `--db DIR`, opens `DIR/cms.db`,
refuses to start on any of the four failures in `DESIGN.md` §13.4, and serves
the session, identity, grant, document, version, diff, history, transition,
workflow, assignment, due-date, and queue routes plus `/healthz`. `earl` can
`login` (with `--dev`), `whoami`, `logout`, `admin grant`, `admin assign`,
`queue [SLUG]`, and
`doc create|show|list|checkout|cancel|edit|checkin|revert|diff|events|transitions|do|assign|due`.

The schema is seven migrations — `0001_users.sql`, `0002_events.sql`,
`0003_identity.sql` (`password_hash`, `roles`, `user_roles`, `sites`, `grants`,
`sessions`), `0004_documents.sql` (`element_types`, `documents`,
`document_versions` with the immutability trigger and the one-open-draft index,
and `grants.document_id`), and `0005_workflow.sql` (`workflows`,
`workflow_states`, `workflow_transitions`, `approvals`, `comments`,
`grants.workflow_id`, the default story workflow, and the rebuild of
`documents`), and `0006_one_workflow_per_kind.sql` (the two partial unique
indexes that make "a site-specific workflow wins over the general one" a rule
rather than a tie-break), and `0007_queue_indexes.sql` (the three indexes M5's
queue queries seek on, replacing the partial `documents_mine` — which could not
answer "unassigned", since SQLite may only use a partial index when the query's
`WHERE` implies the index's). `grants` carries the scope columns whose target table exists; the
rest arrive with the migration that creates theirs, because SQLite cannot add a
foreign key to a column that already exists. `internal/domain` and
`internal/authz` already carry and resolve the whole scope.

0005 is the one migration that carries the `-- migrate: disable-foreign-keys`
directive, and it needs it: `documents` is rebuilt through SQLite's documented
twelve-step `ALTER` procedure — the only way to attach the composite foreign
key `(workflow_id, state) REFERENCES workflow_states(workflow_id, slug)` — and
with enforcement on, `DROP TABLE` performs an implicit `DELETE FROM` that
cascades into `document_versions`.

**The default story workflow is seeded by that migration, not by `cmsdb seed`.**
`documents.workflow_id` is `NOT NULL`, so no document row may exist before a
workflow does, and the rebuild has to place the rows already there. `seed`
reports what it finds; the state machine is written down once, in SQL.

Assignment is not a transition. `Assign`, `Unassign`, and `SetDue` write
`documents.assigned_to` and `documents.due_at` through one store method and
never touch `state`; a transition may still assign as an *effect*, through the
engine. They need `Edit` over the document and deliberately **not** the edit
lease: the lease protects the working draft, and requiring a checkout to hand
work over would mean taking the draft away from the person being handed it.
Saved queue definitions live in `internal/config`, not in the schema
(`DESIGN.md` §14).

Exactly one statement writes `documents.state`: the `UPDATE` inside
`store.ApplyTransition`, which takes the engine's check as a callback and
cannot run without it. `internal/workflow` is its only caller (invariant 4),
all the SQL is still in `internal/store` (invariant 2), and `make lint` plus a
test in `internal/workflow` enforce both. Creating a document is not a
transition — it starts in the initial state rather than moving into it — so
`CreateDocument` writes the column once at `INSERT`.

`internal/{migrate,store,ids,clock,domain,authz,events,service,workflow,api,reqctx}`
are real. `internal/{publish,jobs,render,web}` are still a `doc.go` stating the
package's responsibility and permitted imports — read that doc before adding
the first real file to one.

`--db` names a **directory** that must already exist; the database inside it is
always `cms.db`. Only `cmsdb init` creates a database and only `cmsdb` migrates
one. `cmsd` has no `--migrate` flag, no `--create` flag, and no other way to say
yes.

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
- `internal/workflow` is the only thing that moves a document. Nothing else
  reaches `store.ApplyTransition`, and there is no route, method, or flag that
  sets a state directly.
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
