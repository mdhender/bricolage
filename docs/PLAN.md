# Implementation Plan

Thirteen milestones. Each is independently shippable, independently testable,
and sized so that one agent can complete it in one focused session.

Read `docs/DESIGN.md` first. This document says *when*; that one says *what* and
*why*. Where they disagree, `DESIGN.md` wins and this file gets a fix.

## How to work a milestone

1. Re-read the milestone's section here and the sections of `DESIGN.md` it
   references. Do not start from memory of a previous session.
2. Write the migration first, if the milestone has one. Run `cmsdb migrate up`
   against a scratch database and inspect the result before writing Go.
3. Write `domain` types and pure functions, with tests, before any I/O.
4. Write `store` methods, with tests against a real in-memory database.
5. Write the `service` method, with tests, asserting on emitted events.
6. Write the transport (`api`) and the `earl` command together.
7. Write the end-to-end test named in the acceptance criteria.
8. Run the full gate (below). Only then open a PR.

## Definition of done

A milestone is done when **all** of these hold. There is no partial credit.

- [ ] `go build ./...` succeeds
- [ ] `go vet ./...` is clean
- [ ] `gofmt -l .` prints nothing
- [ ] `go test ./...` passes, including the race detector: `go test -race ./...`
- [ ] Every acceptance criterion in the milestone has an automated test
- [ ] Every new state-changing operation writes an event
- [ ] Every new configurable guard name has an enforcing code path
- [ ] No SQL exists outside `internal/store`
- [ ] No `time.Now()` exists outside `main` and `internal/clock`
- [ ] `docs/DESIGN.md` updated if the design actually changed
- [ ] The milestone's `earl` commands work against a freshly bootstrapped
      database, by hand, once

## Sequencing

```
M0 ─▶ M1 ─▶ M2 ─▶ M3 ─▶ M4 ─▶ M5
                   │      │
                   │      └──▶ M11 ─▶ M12
                   └──▶ M6 ─▶ M7 ─▶ M8 ─▶ M9 ─▶ M10
                                                  │
                                        M13 ◀─────┘
```

M11 (comments, approvals) and M12 (alerts) may proceed in parallel with the
M7–M10 publishing chain once M4 lands. Everything else is a hard dependency.

**M0–M6 is a working editorial system with no publishing.** M0–M9 is a working
CMS. Ship at either boundary if you need to.

---

## M0 — Foundation

**Goal.** A repository that builds, tests, and lints, with the three commands
present and doing nothing but printing their version.

**Work.**
- Set `go.mod` to **Go 1.25**. This is a hard floor:
  `net/http.CrossOriginProtection` arrived in 1.25 and is the CSRF defence.
- Create `cmd/cmsdb`, `cmd/cmsd`, `cmd/earl`, each a cobra root with `version`.
  Cobra is retained.
- Create the `internal/` tree from `DESIGN.md` §4 with a doc.go in each package
  stating its responsibility and its permitted imports.
- **Delete `pkg/way`**, `cli/`, and `cmd/bricolage/`. Nothing replaces the
  router; the route table is `net/http.ServeMux` patterns. Preserve the MIT
  header convention in every new file.
- `cmsd serve` binds `127.0.0.1:18443` by default and speaks plain HTTP. It
  never terminates TLS. See `DESIGN.md` §11, "Serving model".
- Graceful shutdown as one code path, reached by `SIGTERM`, by `--timeout`
  expiry, and later by the dev shutdown route. Write it once.
- `cmsd serve --timeout DURATION` — shut down gracefully after the duration,
  exit 0, log one line naming the configured value. Default `0` = never.
  Ungated: this ships in production builds.
- `internal/web/devroutes`, exposing a single `Register(mux, deps)` that the
  route builder calls **only** when the resolved environment is `development`.
  No build tag gates them — see `DESIGN.md` §11. In M0 it registers only
  `GET|POST /__development/shut-it-down`.
- `internal/config`: the `environment` setting (`development` | `production`,
  default **production**), resolved from `--env`, `$CMS_ENV`, file, default in
  that order. See `DESIGN.md` §14.
- The loopback-peer check; the startup banner printing the environment in both
  environments and the loud warning only in `development`; `WARN` logging on
  every `/__development/*` request.
- `internal/buildenv`: the `//go:build production` / `//go:build !production`
  pair from `DESIGN.md` §14, exporting one `Verify()`. Each `main` calls it
  explicitly — **not** from `init()`, so `main` keeps control of when it runs.
- Add a `Makefile` (or `Taskfile`) with `build`, `test`, `lint`, `check`, a
  `dev` target that runs `go run ./cmd/cmsd serve --env development` (the `dev`
  target must **not** start Caddy — that is a Homebrew service), and a
  `release` target that cross-compiles all three commands with
  `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags production -trimpath`
  into `deploy/linux/amd64/`. Only `release` passes a tag; `go run ./cmd/...`
  must work for every command without one.
- Add CI running the full gate on push, including the no-dev-routes assertion
  and the no-directory-creation assertion
  (`grep -rn 'os\.MkdirAll\|os\.Mkdir(' ./cmd ./internal` prints nothing).
  Both are cheap greps that catch the two rules nobody notices breaking.

**Acceptance.**
1. `make check` passes from a clean clone on Go 1.25.
2. `./cmsdb version`, `./cmsd version`, `./earl version` each print a version
   and exit 0.
3. `go list ./internal/domain` shows no repository-internal imports.
4. `grep -r "pkg/way" .` returns nothing outside git history.
5. With the Homebrew Caddy service running, `cmsd serve` yields a 200 from
   `https://htmx-app.localhost:8443/healthz` with a valid certificate. Never
   run Caddy directly, and never load `deploy/Caddyfile.dev` — it is an example
   only. See `deploy/README.md`.
6. `cmsd serve` with no `--addr` binds loopback. A test asserts the default is
   not `0.0.0.0` or a bare `:port`.
7. `cmsd serve --timeout 2s` exits 0 within a small margin of two seconds,
   having drained in-flight requests. Test with a fake clock where possible and
   a real short duration in the end-to-end test.
8. **With no `--env` and no `CMS_ENV`**, `/__development/shut-it-down` returns
   404. This test gates release.
9. `cmsd routes` with the default environment does not list any
   `/__development/*` pattern, and lists them under `--env development`. The
   routes are absent from the mux, not registered and refused.
10. With `--env development`,
   `curl https://htmx-app.localhost:8443/__development/shut-it-down`
   returns 200 **and the response is fully received** before the process exits;
   the process then exits 0. Assert on the received body, not just the exit
   code — this is the flush-before-shutdown requirement.
11. Every command runs under `go run ./cmd/<name>` with no tag. A CI step runs
   `go run ./cmd/cmsd version`, `./cmd/cmsdb version`, and
   `./cmd/earl version` to keep it that way.
12. An unset, empty, or misspelled environment resolves to `production`. A
   table-driven test covers `""`, `"Development"`, `"dev"`, `"prod"` — only the
   exact string `development` enables anything.
13. `cmsd serve` logs its environment on startup in both environments.
14. The interlock, four cases. Built without the tag: exits 0 with `CMS_ENV`
   unset and with `CMS_ENV=development`, panics with `CMS_ENV=production`.
   Built with `-tags production`: panics with `CMS_ENV` unset, exits 0 with
   `CMS_ENV=production`. Run the tagged half in CI with `-tags production`.
15. `grep -rn "func init" ./cmd ./internal/buildenv` shows no `init()` calling
   `Verify`. The call site is `main`, and a test asserts a `Verify` failure is
   reachable only after `main` has begun.
16. `make release` produces `linux/amd64` binaries; `file deploy/linux/amd64/cmsd`
   confirms the platform and `CGO_ENABLED=0` needed no toolchain.

**Out of scope.** Any behaviour beyond `/healthz` and shutdown. **`cmsd` opens
no database in M0 and has no `--db` flag yet** — it gains one in M1, under the
rules in `DESIGN.md` §13.4. Do not add a database, a directory, or a file
anywhere in this milestone; when the flag arrives it names an existing directory
and the server neither creates nor migrates what it finds there.

---

## M1 — Storage and `cmsdb`

**Goal.** A database file can be created, migrated, and checked — and nothing
except `cmsdb init` can bring one into existence.

Read `DESIGN.md` §13 in full before writing a line of this milestone. Most of it
is about what these commands must refuse to do.

**Work.**
- The first migration. Two files, because two are the fewest that make
  acceptance 6 and 7 mean anything: `0001_users.sql` and `0002_events.sql`.
  `users` carries only the identity columns M2 cannot change — the surrogate
  key, the `uid`, and the email and name `bootstrap admin` is given — and is
  here because every foreign key in the schema eventually points at it.
  `events` is `DESIGN.md` §10 unchanged, and is here because §10 says to wire
  the audit spine from the first milestone that has a schema; its `actor_id`
  foreign key is what acceptance 7's violating insert violates. Nothing writes
  to either table in M1.
- `internal/migrate`: `//go:embed schema/*.sql`, ordered, applied through
  `sqlitemigration`. The application ID is a constant here: `0x434D5330`, the
  ASCII bytes of `CMS0`, `1129141040` decimal, passed as
  `sqlitemigration.Schema.AppID`. Schema version bookkeeping is
  `PRAGMA user_version`, maintained by `sqlitemigration`. **No
  `schema_migrations` table and no hand-rolled version tracking.**
- `internal/store`: two entry points and one shared name.
  - The database file name is the unexported constant `cms.db`. Both entry
    points take a **directory** path and join it.
  - `Create(dir)` — used only by `cmsdb init`. Fails if `dir` does not exist.
    **Never creates a directory.** Opens with explicit flags including
    `OpenCreate`, applies migrations, stamps the application ID.
  - `Open(dir)` — used by every other `cmsdb` subcommand and by `cmsd`. Fails if
    `dir` does not exist, fails if `cms.db` does not exist, and **never names
    `OpenCreate`**. Verifies the application ID and the schema version; the
    caller says whether a version behind the binary is an error (`cmsd`) or a
    prompt to migrate (`cmsdb migrate up`).
  - Pool construction per `DESIGN.md` §13.3 — WAL on persistent stores,
    `foreign_keys=ON` on every connection of every store including in-memory,
    `busy_timeout`, a read pool and one serialized writer. Explicit open flags
    everywhere; the zero value creates.
- `internal/clock`: `Clock`, `Real`, `Fake`.
- `internal/ids`: ULID generation.
- `cmsdb init`, `cmsdb migrate status|up [--to N]`, `cmsdb check`, `cmsdb vacuum`.
- `cmsdb check` runs `PRAGMA foreign_key_check`, `PRAGMA integrity_check`, and
  reports stuck job leases and orphaned resources (both empty for now). It also
  reports the application ID and schema version it found.
- `cmsd serve` gains `--db DIR`. It calls `Open`, requires an exact schema
  version match, and exits non-zero without a listener on any failure. It does
  not gain a `--migrate` flag, a `--create` flag, or any other way to say yes.

**Acceptance.**
1. `mkdir -p /tmp/x && cmsdb init --db /tmp/x` creates `/tmp/x/cms.db`; running
   it twice is safe and reports "already initialised".
2. **`cmsdb init --db /tmp/does-not-exist` exits non-zero, names the directory,
   and creates nothing.** A test asserts the directory still does not exist
   afterwards. The same test exists for every other `cmsdb` subcommand and for
   `cmsd serve`.
3. **`grep -rn "MkdirAll\|os.Mkdir" ./internal ./cmd` returns nothing.** This is
   a CI step, not a habit.
4. A freshly initialised database has `PRAGMA application_id = 0x434D5330` and
   `PRAGMA user_version` equal to the number of embedded migrations. Assert both
   by reading the pragmas from a second, independent connection.
5. `cmsdb migrate status` lists applied and pending migrations, and prints the
   application ID and version it read.
6. Applying migrations to an empty database and to a partially-migrated one both
   converge to the same schema. Test by comparing `sqlite_schema` dumps.
7. A store test opens an in-memory database, applies all migrations, and writes
   and reads one row. A second test asserts `PRAGMA foreign_keys` is `1` on a
   connection drawn from the in-memory pool, and that a violating insert is
   rejected by result code.
8. `PRAGMA journal_mode` is `wal` on a persistent store.
9. Concurrent writers do not produce `SQLITE_BUSY`: a test spawning 20
   goroutines each doing 50 writes passes under `-race`.
10. **`cmsd serve` against a directory with no `cms.db` exits non-zero and binds
   nothing.** Assert that no file was created and that nothing is listening.
11. **`cmsd serve` against a file whose `application_id` is wrong exits non-zero
   and binds nothing.** Two variants, both real failure modes: a zero-length
   file (`application_id` 0, which `sqlitemigration` would have adopted), and a
   valid SQLite database stamped with a different ID.
12. **`cmsd serve` against a database one migration behind exits non-zero,
   binds nothing, and leaves `user_version` unchanged.** The same for a database
   one migration ahead. Assert the version afterwards — a server that migrated
   and then failed for another reason must not pass this test.
13. The error message for each of 10–12 names the expected and the actual value
   and the path it opened. A test asserts on the message, because these are the
   messages an operator reads at three in the morning.

**Out of scope.** Any domain table beyond what the first migration needs — see
the first bullet above for what that turned out to be. A schema-version-tracking
table of our own — the pragmas are the bookkeeping.

---

## M2 — Identity, roles, grants, sessions

**Goal.** A person can be created, can log in, and their effective privilege
over a scope can be computed.

**Schema.** `roles`, `user_roles`, `grants`, `sessions`, and the credential
columns on `users`, whose identity columns M1's first migration already created.
`sessions(id, user_id, token_sha256, created_at, expires_at, last_seen_at)`.

**Work.**
- `internal/domain`: `Privilege` with the ordered scale and `DENY = 255`;
  `Grant`, `Scope`, `Subject`.
- `internal/authz`: `Resolve(grants, subj) Privilege` — `MAX`, deny wins. Pure,
  no I/O. Scope matching including `category_deep` prefix semantics.
- Anti-escalation check in the grant-writing path (`DESIGN.md` §7.3).
- Password hashing, token generation, session store.
- `cmsdb bootstrap admin`, `cmsdb seed` (roles, one site, default workflow).
- `cmsd serve` with `POST /api/v1/sessions`, `GET /api/v1/me`, auth middleware.
- `earl login`, `earl whoami`. `earl` defaults to the public origin
  (`https://htmx-app.localhost:8443` in development), not the Go listener.
- Trusted-proxy middleware resolving the client IP from `X-Forwarded-For` only
  when the peer is in `server.trusted_proxies`; the result goes on the context
  and is read from there, never re-parsed.
- Session cookies `Secure`, `HttpOnly`, `SameSite=Lax`, unconditionally.
- `GET /__development/log-me-in/{email}?returnTo={url}` in `devroutes`, per
  `DESIGN.md` §11: existing users only, `returnTo` validated, writes a
  `session.dev_login` event, content-negotiated response.
- `earl login --dev --email E` using that route, so an agent never needs to
  construct the request by hand. The flag is client-side: it selects the dev
  login route, and fails with a clear message if the server is not in
  `development`.

**Acceptance.**
1. `cmsdb bootstrap admin --email a@b.c --name Admin` prints a generated
   password exactly once; a second run exits non-zero and changes nothing.
2. A password supplied on a command-line flag is rejected outright.
3. `earl login` then `earl whoami` prints the bootstrapped admin, over the
   Caddy proxy rather than direct to the listener.
4. The session cookie carries `Secure` even though the test server speaks plain
   HTTP. A test asserts the attribute directly. An `r.TLS != nil` guard anywhere
   in the cookie path is a failure, whatever the tests say.
5. `X-Forwarded-For` from an untrusted peer is ignored and the peer address is
   used; the same header from `127.0.0.1` is honoured. Two tests.
6. Table-driven `Resolve` tests cover: no grants → 0; two grants → max; a `DENY`
   anywhere → `DENY`; `category_deep=1` matches a descendant; `category_deep=0`
   does not; every `NULL` column behaves as a wildcard.
7. A user holding `EDIT` on a scope cannot create a `PUBLISH` grant on it; the
   attempt returns 403 and writes nothing.
8. An expired session returns 401.
9. `/__development/log-me-in/{email}` returns 404 with the default
   environment, 404 under `--env production`, and issues a session under
   `--env development`. Three tests; the first gates release.
10. The session it issues carries exactly the user's real roles and grants —
   no elevation. Assert by comparing `Resolve` output against a
   password-authenticated session for the same user.
11. An unknown email returns 404 and creates no account.
12. `returnTo` is rejected with 400 when it names another origin, and accepted
   when relative or equal to `server.public_origin`. Table-driven, including
   `//evil.example.com` and `https://evil.example.com@localhost`.
13. Every dev login writes a `session.dev_login` event carrying the email and
   the peer address.

**Out of scope.** Documents. Role administration UI.

---

## M3 — Documents, versions, events

**Goal.** The full checkout / edit / check-in / revert / diff cycle, with every
step audited.

**Schema.** `sites` (from M2 seed), `element_types` (minimal: key_name, name,
kind, `schema` accepted but not yet validated), `documents`,
`document_versions` plus the immutability trigger, `events`.

**Work.**
- `internal/domain`: `Document`, `Version`, `Lock` with `IsHeldBy(user, now)`.
- `internal/events`: type constants, `Record(tx, Event)`, subject history query.
- `internal/service`: `CreateDocument`, `Checkout`, `Checkin`, `CancelCheckout`,
  `Revert`, `UpdateDraft`, `GetVersion`, `Diff`.
- Word-level diff between any two versions. Use a small dependency or write it;
  either is fine, but the output must be stable and golden-tested.
- API: the document, version, and diff routes from `DESIGN.md` §12.
- `earl doc create|show|list|checkout|checkin|revert|edit|diff|events`.

**Acceptance.**
1. Create → checkout → edit → checkin produces version 1 with
   `checked_in_at` set; the working draft row is gone or superseded.
2. `UPDATE` on a checked-in version fails with the trigger's message. Test at
   the store level.
3. Two concurrent `Checkout` calls: exactly one succeeds, the other gets 409.
4. A lock whose `lock_expires_at` has passed does not block a new checkout.
5. `Revert` on a document with no checked-in version deletes it; on a document
   with one or more, it discards the draft and leaves the latest intact.
6. Every one of the above writes exactly one event of the expected type, with
   actor and payload. Assert this in service tests, not by reading the log.
7. `earl doc diff UID --from 1 --to 2` matches a golden file.

**Out of scope.** Workflow state. Categories. Publishing.

---

## M4 — The workflow engine

**Goal.** Documents move between states, and only ever through a declared
transition that the actor is permitted to make.

**Schema.** `workflows`, `workflow_states`, `workflow_transitions`, `approvals`
(table only; the guard lands here, the API in M11).

**Work.**
- `internal/workflow`: `Guard`, `Effect`, `Transition`, `Engine`,
  `Available`, `Do`, and the unexported `check` they share.
- All seven guards from `DESIGN.md` §6.1, each with enforcement.
- All four effects.
- `Do` in one transaction: check → update → event → enqueue.
- `cmsdb seed` gains the default story workflow from `DESIGN.md` §5.4.
- API: `GET`/`POST /api/v1/documents/{uid}/transitions`.
- `earl doc transitions`, `earl doc do`.

**Acceptance.**
1. `GET .../transitions` lists every transition out of the current state,
   including refused ones with a `reason`.
2. `POST` with a `to` that is not a declared transition from the current state
   returns 409 — **including** when the caller is an administrator.
3. For each of the seven guards: one test where it passes, one where it refuses,
   and the refusal names the guard.
4. A grep for writes to `documents.state` finds them only inside
   `internal/workflow`. Enforce with a test that walks the AST, or a CI grep.
5. `Available` and `Do` disagree in no case. Property test: for every transition
   `Available` marks permitted, `Do` succeeds; for every one it refuses, `Do`
   returns the same guard error.
6. A failed guard rolls back cleanly: state, events, and jobs are all unchanged.
7. `approvals_met` counts approvals for the **current version** only; adding a
   new version drops the count to zero.

**Out of scope.** Publishing from `publishable` states. Comment resolution
guard may be stubbed to "no comments exist" until M11 — but the guard must
exist and be enforced, not merely named.

---

## M5 — Assignment, due dates, queues

**Goal.** Work can be given to a person and found again.

**Schema.** Already present on `documents` (`assigned_to`, `due_at`).

**Work.**
- `internal/service`: `Assign`, `Unassign`, `SetDue`.
- Queue queries: by state, by assignee, unassigned, overdue, by site, combined.
- Saved queue definitions in config (not schema).
- API: assignment routes, `GET /api/v1/queues/{slug}`, filters on
  `GET /api/v1/documents`.
- `earl doc assign`, `earl doc list --state --assignee --unassigned --overdue`,
  `earl queue`.

**Acceptance.**
1. `earl doc list --state review --unassigned` returns exactly the documents in
   review with no assignee. This is the query the Perl system could not express;
   it gets a named test.
2. Assignment and due-date changes each write an event.
3. `EffectAssignToActor` and `EffectClearAssignee` fire on transitions
   configured to use them.
4. Queue queries use an index: assert with `EXPLAIN QUERY PLAN` in a test that
   no query in the queue path performs a full scan of `documents`.
5. Overdue is computed against the injected clock, not wall time.

**Out of scope.** Notifications about assignment (M12).

---

## M6 — Jobs

**Goal.** Work can be scheduled, claimed exactly once, retried, and recovered
after a worker dies.

**Schema.** `jobs`.

**Work.**
- `internal/jobs`: `Queue.Enqueue`, `Queue.Claim`, `Queue.Complete`,
  `Queue.Fail`, `Queue.ExtendLease`; a `Handler` registry keyed by kind; a
  worker loop with backoff.
- A `noop` job kind for testing.
- Workers hosted in `cmsd` behind `--workers N`.
- API: `GET /api/v1/jobs`, `POST /api/v1/jobs/{id}/retry`.
- `earl job list [--failed|--pending]`, `earl job retry`.
- `cmsdb check` now reports leases held past expiry.

**Acceptance.**
1. Twenty goroutines call `Claim` against one ready job; exactly one gets it.
   Run under `-race`.
2. A claimed job whose lease expires is re-claimable, and `attempts` is 2.
3. A job failing `max_attempts` times sets `failed_at` and `last_error` and is
   not claimed again.
4. `Claim` returns jobs in priority then schedule order. Test with a mixed set.
5. A job scheduled for the future is not claimed until the fake clock passes it.
6. Graceful shutdown releases the lease of an in-flight job rather than
   abandoning it.

**Out of scope.** Publish jobs specifically.

---

## M7 — Sites, categories, element types, URIs

**Goal.** Documents have a place in a hierarchy and a computable address.

**Schema.** `categories` with materialised `path`, `document_categories`,
`output_channels`, `collections`, `document_collections`; `element_types.schema`
now enforced.

**Work.**
- Category CRUD with `path` maintenance, including subtree rewrite on move.
- `domain.ValidateContent(elementType, content)`.
- A strftime formatter in `domain` supporting the format tokens Bricolage used,
  plus `%{categories}` and `%{slug}`.
- `domain.BuildURI(doc, version, category, oc)` — pure.
- API and `earl` for categories, element types, output channels.

**Acceptance.**
1. Moving a category rewrites every descendant `path` in one statement; a test
   with three levels and two siblings verifies every row.
2. `path` always begins and ends with `/`; the root is `/`. Property test.
3. URI construction test vectors, golden: root category, nested category, slug
   on and off, fixed URI format, a format containing `%Y/%m/%d`, and a document
   with no cover date.
4. The category token substitution consumes the following slash, so no URI ever
   contains `//`. Property test over random category depths.
5. Check-in of content that violates the element type schema returns 422 and
   names the offending fields. A working draft may be invalid.
6. `category_deep` grants resolve correctly against the new `path` column —
   re-run the M2 authz tests against real categories.

**Out of scope.** Rendering.

---

## M8 — Rendering and preview

**Goal.** A document can be turned into bytes.

**Work.**
- `internal/render`: template lookup walking up the category tree, first match
  wins; `html/template` execution with a document context.
- Three modes: publish, preview, validate.
- Preview writes to a scratch tree and is served by `cmsd` under `/preview/`.
- Media blobs content-addressed at `blobs/<sha[:2]>/<sha>`.
- `earl` preview command opening or printing the rendered output.

**Acceptance.**
1. Template cascade: a document in `/features/film/` finds a template at
   `/features/` when none exists at `/features/film/`, and one at `/` when
   neither exists. Three tests, plus one for "no template anywhere" → clean
   error naming the element type and the searched paths.
2. Validate mode reports template parse errors without writing anything.
3. Rendering is deterministic: same version, same template, same bytes. Golden.
4. Preview of a checked-out draft renders the draft; preview of a checked-in
   document renders the current version.
5. A template that errors at execution time produces a 500 with the template
   name and line, and writes no partial file.

**Out of scope.** Distribution to remote servers.

---

## M9 — Publishing, resources, stale expiry

**Goal.** Publishing works, is schedulable against a fixed version, and cleans
up after itself.

**Schema.** `published_resources`.

**Work.**
- A `publish` job kind whose payload names `document_version_id`.
- An `expire` job kind that deletes a resource.
- `internal/publish`: render to the output tree, record resources, diff against
  stored resources, enqueue expiry for URIs no longer produced.
- Unique-violation detection via `SQLITE_CONSTRAINT_UNIQUE`, never error text.
- Transition effect: publishing from a `publishable` state.
- API: `POST /api/v1/documents/{uid}/publications`,
  `GET /api/v1/documents/{uid}/resources`.
- `earl publish [--at]`.

**Acceptance.**
1. **The pinning test.** Schedule version 5 for T. Check out, edit, check in
   version 6. Advance the fake clock past T. Version 5 is what appears in the
   output tree. This is the single most important test in the project.
2. Republishing after a slug change deletes the file at the old URI and creates
   the new one. Assert on both the filesystem and `published_resources`.
3. Republishing after a category change does the same.
4. Two documents resolving to the same URI: the second publish fails with a
   clear "URI already in use" naming the other document, detected by result
   code.
5. `documents.live_version_id` is set on success and unchanged on failure.
6. A publish failure leaves no partial output and no orphaned resource rows.
7. `cmsdb check` reports a resource row with no file on disk, and a file with no
   row.

**Out of scope.** Related assets. Remote distribution.

---

## M10 — Related-asset cascade

**Goal.** Publishing a document publishes what it depends on, safely.

**Work.**
- Reference extraction from `document_versions.content`.
- `publish.Gather(graph, root, actor) (set []Node, refusals []Refusal)` — a pure
  function over a loaded graph, per `DESIGN.md` §8.2.
- Cycle safety, per-node permission, state gate, lock gate.
- `publish.related_failure` config: `fail` | `warn`.
- `--dry-run` returning the gathered set and every refusal with its reason.
- `earl publish --dry-run`.

**Acceptance.**
1. A cycle (A references B references A) terminates and publishes each once.
2. A related document the actor cannot publish is refused **by name**, and under
   `fail` nothing at all is published.
3. Under `warn`, the root publishes and the refusal is reported.
4. A related document in a non-publishable state is refused by name.
5. A checked-out related document is refused by name.
6. `--dry-run` schedules no jobs and writes no events, and its output matches
   what a real publish would do. Assert by running both and comparing.
7. Depth: a chain of five documents publishes all five.

---

## M11 — Comments and approvals

**Goal.** People can talk about a document and sign off on it.

**Schema.** `comments` (from M3 tables if deferred), `approvals` API.

**Work.**
- Comment threads, replies, resolution.
- Approve and withdraw approval.
- Wire `GuardCommentsResolved` and `GuardApprovalsMet` to real data, replacing
  any M4 stub.
- API and `earl` commands.

**Acceptance.**
1. Approving twice as the same user on the same version is idempotent, not an
   error and not two rows.
2. Two distinct users approving satisfies `required_approvals = 2`.
3. Checking in a new version drops the approval count to zero for the new
   version; the old version's approvals remain queryable.
4. `EffectClearApprovals` on a transition removes them explicitly.
5. An unresolved comment blocks a transition guarded by
   `comments_resolved`, and the refusal names the open thread count.
6. Every comment and approval writes an event.

---

## M12 — Alerts and notifications

**Goal.** People find out that something happened.

**Schema.** `alert_rules`, `notifications`.

**Work.**
- Condition evaluation resolving fields from actor, event payload, then subject,
  in that order.
- Operators `eq ne lt lte gt gte in contains matches`; `matches` compiled and
  validated at rule-save time.
- Rules evaluated **after commit**, off the event row.
- In-app notifications with read state; e-mail channel behind an interface with
  a logging implementation as the default.
- API and `earl` for rules and notifications.

**Acceptance.**
1. A rule matching `document.transitioned` with `to_state = 'legal'` notifies
   the configured recipients and nobody else.
2. All conditions must pass (`AND`). A rule with two conditions where one fails
   fires nothing.
3. A rule with an invalid regexp is rejected at save time with a clear message,
   not at fire time.
4. A failing notification channel does not roll back the transition that caused
   it. Test with a channel that always errors.
5. Field resolution order is asserted: a field present on both the event payload
   and the subject resolves from the payload.

---

## M13 — Web UI

**Goal.** Editors can do their job in a browser.

**Work.**
- `internal/web`: `html/template`, HTMX, server-rendered partials.
- Screens: queue list, document view, editor, version history and diff, desk
  actions, comments, notifications, admin for workflows / roles / grants.
- Every action button rendered from `workflow.Available`, showing refusal
  reasons on disabled actions.
- CSRF via `net/http.CrossOriginProtection`, with `server.public_origin`
  registered as a trusted origin. It wraps cookie-authenticated routes;
  bearer-token API routes are exempt, because a browser does not attach a
  bearer token ambiguously.
- Session cookie auth alongside bearer tokens.

**Acceptance.**
1. No handler in `internal/web` calls `internal/store` directly.
2. The action bar on a document is generated from `Available`; a manually forged
   `POST` for a refused transition returns 409. Test both.
3. A cross-origin `POST` to a cookie-authenticated route is rejected; the same
   request with the correct `Origin` succeeds; a bearer-token request with no
   `Origin` at all succeeds. Three tests.
4. Every page renders with an empty database without panicking.
5. The UI performs no operation that `earl` cannot also perform.

---

## Cut lines

If scope must shrink, cut in this order, and say so in the release notes:

1. M13 — the API and `earl` are a usable product for a technical team.
2. M12 — the event log still records everything; people just have to look.
3. M10 — single-document publishing is useful; relatives are manual.
4. M11 — approvals can be modelled as extra states in the interim.

**Never cut:** the version-pinning behaviour in M9, the shared `check` in M4,
the lease in M6, or the resource diff in M9. Those are the four things that make
this worth building rather than reaching for an off-the-shelf CMS.
