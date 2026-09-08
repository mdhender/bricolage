# Design

This document describes what we are building and why. It is the reference for
every implementation decision in this repository. `docs/PLAN.md` sequences the
work; `AGENTS.md` states the rules you must follow while doing it.

## 1. What this is

A content management system whose distinguishing feature is that it manages
**editorial workflow and publishing** well: many people moving documents through
review to publication, on schedule, to multiple destinations, without losing
track of who did what.

It is a greenfield Go program. It is **not** a port.

The Perl Bricolage CMS is a *source of requirements and hard-won lessons*, not a
specification. We reimplement behaviour we judged worth having and discard the
rest. Where this document says "Bricolage does X", that is evidence about the
problem domain, not an instruction to copy an implementation.

Non-goals, stated up front so they do not creep in:

- **No SOAP.** Clients use the REST API. There are no existing clients to break.
- **No migration from a Bricolage database.** No schema compatibility, no
  preserved integer ids, no import tooling. If this is ever needed it will be a
  separate program.
- **No mod_perl-era concepts.** No Mason, no burners-as-plugins, no
  group-to-group permission algebra, no EAV attribute tables, no runtime class
  registry.

The background research that produced these judgements lives in the
`bricoleurs` workspace as a Hugo site. The two most relevant pages are the
workflow assessment and the permissions design. You do not need to read them to
implement this document, but they are where the "why" is written down.

## 2. Constraints

| Constraint | Value |
|---|---|
| Language | **Go 1.25 or later** — this is a hard floor, see below |
| Database | SQLite, via `zombiezen.com/go/sqlite` |
| Migrations | `zombiezen.com/go/sqlite/sqlitemigration` |
| HTTP routing | standard library `net/http.ServeMux` only — **no third-party router** |
| CSRF | standard library `net/http.CrossOriginProtection` (Go 1.25) |
| Server-rendered UI | `html/template` + HTMX, as an API client (§3) |
| Markdown, if needed | `github.com/yuin/goldmark` |
| CLI framework | `github.com/spf13/cobra` — retained, already a dependency |
| TLS | terminated by a reverse proxy, never by `cmsd` (§11) |
| Licence | MIT; every Go file carries the existing header |

Go 1.25 is the floor because `net/http.CrossOriginProtection` arrived in it.
That is the CSRF defence for the HTMX UI and there is no reason to reimplement
it. Do not lower the floor to accommodate an older toolchain.

`pkg/way`, the third-party router in the repository today, is **removed and not
replaced.** `net/http.ServeMux` has had method and wildcard patterns since
Go 1.22, which is everything the route table in §12 needs. Delete the package;
if any fragment survives, its attribution and licence survive with it.

`cobra` stays. Three commands with subcommands is what it is for.

> Confirm exact method names with `go doc net/http.CrossOriginProtection`
> before wiring it. The type exists in 1.25; the surface is small but worth
> reading rather than guessing.

SQLite is a deliberate choice, not a placeholder. A single-node CMS with a
handful of editors and a background publisher has no workload that needs a
server database, and SQLite removes an entire class of operational burden. The
design must not depend on features SQLite lacks, and must respect its
single-writer model (§13).

`cmsd` never terminates TLS and never listens on a public interface. It speaks
plain HTTP on loopback behind Caddy or nginx, which guarantees TLS 1.3 or
better. This has consequences for cookies, client addresses, and origin
checking that are easy to get wrong — see §11.

## 3. Architecture

Four layers with a strict, one-directional dependency rule:

```
  transport   internal/api  (JSON)      internal/web  (HTML/HTMX)
                     \                        /
  service             internal/service  ────┘
                              |
  domain              internal/domain   (types, invariants, pure functions)
                              |
  storage             internal/store    (SQL, zombiezen)
```

**The rule: dependencies point downward only.**

- `domain` imports nothing from this repository. No `database/sql`, no
  `net/http`, no `time.Now()`. It is types, constants, and pure functions that
  decide things.
- `store` imports `domain`. It converts between rows and domain types. It
  contains **all** SQL and no business decisions.
- `service` imports `domain` and `store`. It owns transactions, orchestrates
  operations, writes events, and enqueues jobs. **This is where a "use case"
  lives.**
- `api` and `web` import `service`. They parse requests, call one service
  method, and render a response. They contain no business logic and never touch
  `store` directly.

If you find yourself wanting to break this rule, the answer is almost always
that a decision belongs in `domain` as a pure function that both layers call.

### Why this shape

Because `earl` and the HTMX UI must be able to do exactly the same things. If
business logic lives in HTTP handlers, the CLI drifts from the UI and the two
disagree about what is allowed. One service layer, two transports, and the CLI
is a client of the same API the UI uses.

**The HTMX UI is a client, not the application.** `internal/web` renders HTML
fragments by calling the same service methods `internal/api` serialises to JSON.
It holds no state the API does not have and permits no operation the API does
not expose. The test for this is in `docs/PLAN.md` M13: the UI must perform no
operation `earl` cannot also perform. Keeping that true is what stops a
second, accidental application growing inside the templates.

This is also the structural fix for the worst bug in the Perl original: its
permission check for moving a document between desks lived only in the template
that rendered the menu, while the handler took the destination straight off the
request parameter and did no check at all. Here, the function that says what you
may do and the function that does it are the same function (§6.2).

## 4. Package layout

```
cmd/
  cmsdb/            database lifecycle command
  cmsd/             server command
  earl/             API client command
internal/
  domain/           entities, value types, invariants; no I/O
  store/            SQLite persistence; one file per aggregate
  migrate/          embedded migration SQL + runner
  service/          use cases; owns transactions
  workflow/         the transition engine (guards, effects, availability)
  authz/            grants, scope matching, privilege resolution
  publish/          rendering, resources, expiry, related-asset cascade
  jobs/             queue, leases, worker loop, job kinds
  events/           event recording, alert rule evaluation, notifications
  render/           template lookup + execution
  api/              JSON REST handlers, request/response types
  web/              HTMX handlers and html/template files
  web/devroutes/    build-tagged `/__development/*` handlers; stub when untagged
  clock/            Clock interface and implementations
  ids/              external identifier generation
  config/           configuration loading and defaults
schema/             .sql migration files, embedded via go:embed
deploy/             reverse-proxy configuration; Caddyfile.dev
testdata/           fixtures
```

`workflow`, `authz`, `publish`, `jobs`, `events` sit conceptually beside
`service`: they hold logic too specific to be `domain` and too reusable to be a
single service method. They may import `domain` and `store`, not `api` or `web`.

### Existing code

- **`pkg/way/` is deleted.** It is a third-party router superseded by
  `net/http.ServeMux`, which has had method and wildcard patterns since
  Go 1.22. Nothing replaces it — the route table is `ServeMux` patterns. If any
  fragment is kept for any reason, its licence header and attribution are kept
  with it.
- **`cobra` is retained.** Three commands with subcommands is what it is for.
- `cli/` and `cmd/bricolage/` are a single-binary skeleton. They are superseded
  by the three commands in §11. Reuse the cobra patterns; do not preserve the
  structure.
- New code goes in `internal/`, not `pkg/`. Nothing here is meant to be imported
  by another module.

## 5. Domain model

### 5.1 Documents and versions

The one structural idea worth taking from Bricolage unchanged: **identity and
version are separate rows.** A document is a stable identity with a state; a
version is an immutable snapshot of content.

```sql
CREATE TABLE documents (
  id                 INTEGER PRIMARY KEY,
  uid                TEXT    NOT NULL UNIQUE,        -- external identifier
  site_id            INTEGER NOT NULL REFERENCES sites(id),
  kind               TEXT    NOT NULL,               -- 'story' | 'media' | 'template'
  element_type_id    INTEGER NOT NULL REFERENCES element_types(id),
  workflow_id        INTEGER NOT NULL REFERENCES workflows(id),
  state              TEXT    NOT NULL,

  assigned_to        INTEGER          REFERENCES users(id),
  due_at             TEXT,

  locked_by          INTEGER          REFERENCES users(id),
  lock_expires_at    TEXT,

  current_version_id INTEGER          REFERENCES document_versions(id),
  live_version_id    INTEGER          REFERENCES document_versions(id),

  created_at         TEXT    NOT NULL,
  updated_at         TEXT    NOT NULL,

  FOREIGN KEY (workflow_id, state)
    REFERENCES workflow_states(workflow_id, slug)
) STRICT;

CREATE INDEX documents_queue   ON documents(workflow_id, state, due_at);
CREATE INDEX documents_mine    ON documents(assigned_to) WHERE assigned_to IS NOT NULL;
CREATE INDEX documents_overdue ON documents(due_at)      WHERE due_at IS NOT NULL;
CREATE INDEX documents_live    ON documents(live_version_id) WHERE live_version_id IS NOT NULL;
```

```sql
CREATE TABLE document_versions (
  id            INTEGER PRIMARY KEY,
  document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  version       INTEGER NOT NULL,               -- 1-based, monotonic per document
  title         TEXT    NOT NULL,
  slug          TEXT    NOT NULL DEFAULT '',
  cover_date    TEXT,
  content       TEXT    NOT NULL DEFAULT '{}',  -- JSON element tree
  note          TEXT,                           -- check-in message
  created_by    INTEGER NOT NULL REFERENCES users(id),
  created_at    TEXT    NOT NULL,
  checked_in_at TEXT,                           -- NULL = open working draft
  UNIQUE (document_id, version)
) STRICT;

CREATE TRIGGER document_versions_immutable
BEFORE UPDATE ON document_versions
WHEN OLD.checked_in_at IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'checked-in versions are immutable');
END;
```

Three defects in the original die here, by construction rather than by care:

- **The edit lock lives on the document row.** In Bricolage the lock flag lived
  on the version row, so every non-current version reported `checked_out = 0`
  even while the document was locked, and the code had to test a different
  column to compensate. One lock, one row, no lying.
- **There is no version 0.** "Never checked in" is `checked_in_at IS NULL`.
  Nothing needs to know that reverting a never-saved document means deleting it.
- **Immutability is enforced by the database.** A scheduled publish pins a
  version id and can trust it. The trigger also blocks well-meaning
  administrative `UPDATE`s; that is the intended trade. Provide a documented
  `sqlite3` escape hatch, never a code path.

The lock is a **lease**: `lock_expires_at` is set on checkout and extended by
activity. An expired lock is not a lock. This means an editor who closes their
laptop does not block a document forever, which in Bricolage required an
administrator.

### 5.2 Content and element types

Bricolage stored custom fields in eighteen EAV tables. We store a JSON document.

```sql
CREATE TABLE element_types (
  id         INTEGER PRIMARY KEY,
  uid        TEXT    NOT NULL UNIQUE,
  key_name   TEXT    NOT NULL UNIQUE,
  name       TEXT    NOT NULL,
  kind       TEXT    NOT NULL,      -- which document kind it applies to
  top_level  INTEGER NOT NULL DEFAULT 0,
  fixed_uri  INTEGER NOT NULL DEFAULT 0,
  paginated  INTEGER NOT NULL DEFAULT 0,
  schema     TEXT    NOT NULL,      -- JSON: field definitions
  created_at TEXT    NOT NULL
) STRICT;
```

`element_types.schema` declares fields (name, type, repeatable, required,
allowed child element types). `document_versions.content` holds a tree that must
validate against it. Validation is a pure function in `domain`:

```go
func ValidateContent(et *ElementType, content json.RawMessage) []FieldError
```

Validate on check-in, not on every keystroke. A working draft may be invalid; a
checked-in version may not.

### 5.3 Sites, categories, output channels

```sql
CREATE TABLE sites (
  id INTEGER PRIMARY KEY, uid TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL, domain TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1
) STRICT;

CREATE TABLE categories (
  id        INTEGER PRIMARY KEY,
  uid       TEXT    NOT NULL UNIQUE,
  site_id   INTEGER NOT NULL REFERENCES sites(id),
  parent_id INTEGER          REFERENCES categories(id),
  directory TEXT    NOT NULL,        -- one path segment
  path      TEXT    NOT NULL,        -- materialised, always '/'-terminated
  name      TEXT    NOT NULL,
  UNIQUE (site_id, path)
) STRICT;

CREATE TABLE document_categories (
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  category_id INTEGER NOT NULL REFERENCES categories(id),
  primary_cat INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (document_id, category_id)
) STRICT;
```

`categories.path` is materialised and always ends in `/` — `/features/film/`,
and the root is `/`. Two things depend on it: URI construction, and permission
scope matching by subtree prefix (§7). Rebuild descendants' paths when a
category moves; do it in one statement and cover it with a test.

Output channels carry the URI format:

```sql
CREATE TABLE output_channels (
  id               INTEGER PRIMARY KEY,
  uid              TEXT    NOT NULL UNIQUE,
  site_id          INTEGER NOT NULL REFERENCES sites(id),
  name             TEXT    NOT NULL,
  protocol         TEXT    NOT NULL DEFAULT 'https://',
  filename         TEXT    NOT NULL DEFAULT 'index',
  file_ext         TEXT    NOT NULL DEFAULT 'html',
  uri_format       TEXT    NOT NULL,   -- strftime + %{categories} %{slug}
  fixed_uri_format TEXT    NOT NULL,
  use_slug         INTEGER NOT NULL DEFAULT 0,
  uri_case         TEXT    NOT NULL DEFAULT 'mixed'
) STRICT;
```

URI formats are **strftime strings** with `%{categories}` and `%{slug}`
extensions, expanded against the document's cover date. Go's reference-time
layouts cannot express them. Write a small strftime formatter in `domain`; do
not try to translate formats into Go layouts. The category segment substitution
deliberately consumes the following slash, because category paths already end in
one.

### 5.4 Workflow definition

```sql
CREATE TABLE workflows (
  id            INTEGER PRIMARY KEY,
  uid           TEXT    NOT NULL UNIQUE,
  site_id       INTEGER          REFERENCES sites(id),   -- NULL = all sites
  kind          TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  initial_state TEXT    NOT NULL
) STRICT;

CREATE TABLE workflow_states (
  workflow_id        INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  slug               TEXT    NOT NULL,
  name               TEXT    NOT NULL,
  position           INTEGER NOT NULL,
  publishable        INTEGER NOT NULL DEFAULT 0,
  terminal           INTEGER NOT NULL DEFAULT 0,
  required_approvals INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (workflow_id, slug)
) STRICT;

CREATE TABLE workflow_transitions (
  id          INTEGER PRIMARY KEY,
  workflow_id INTEGER NOT NULL,
  from_state  TEXT    NOT NULL,
  to_state    TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  privilege   INTEGER NOT NULL,
  guards      TEXT    NOT NULL DEFAULT '[]',
  effects     TEXT    NOT NULL DEFAULT '{}',
  position    INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (workflow_id, from_state) REFERENCES workflow_states(workflow_id, slug),
  FOREIGN KEY (workflow_id, to_state)   REFERENCES workflow_states(workflow_id, slug),
  UNIQUE (workflow_id, from_state, to_state)
) STRICT;
```

`workflow_states.position` is the column Bricolage never had. Its desk ordering
was derived by sorting start-first, publish-last, and everything else by primary
key — so "the order of the desks" was three buckets. Storing it costs one
integer.

Default story workflow, seeded by `cmsdb`:

```
draft ──submit──▶ review ──approve──▶ approved ──publish──▶ published
  ▲                  │                    │                     │
  └────reject────────┘                    │                     │
  ◀────────────revoke─────────────────────┘                     │
  ◀────────────revise───────────────────────────────────────────┘
```

Plus `archive` from `draft`/`review`/`published`, and `restore` from `archived`.

Note that `published` is a **state, not an exit**. Bricolage removed a published
document from workflow entirely, so a live document was nowhere. Here it stays,
and `documents.live_version_id` independently records what is serving. A
document can sit in `draft` being revised while the previously published version
continues to serve — which is what actually happens.

### 5.5 Supporting tables

```sql
CREATE TABLE approvals (
  id          INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  version_id  INTEGER NOT NULL REFERENCES document_versions(id) ON DELETE CASCADE,
  state       TEXT    NOT NULL,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  created_at  TEXT    NOT NULL,
  UNIQUE (version_id, state, user_id)
) STRICT;

CREATE TABLE comments (
  id          INTEGER PRIMARY KEY,
  uid         TEXT    NOT NULL UNIQUE,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  version_id  INTEGER          REFERENCES document_versions(id),
  in_reply_to INTEGER          REFERENCES comments(id),
  author_id   INTEGER NOT NULL REFERENCES users(id),
  body        TEXT    NOT NULL,
  resolved_at TEXT,
  resolved_by INTEGER          REFERENCES users(id),
  created_at  TEXT    NOT NULL
) STRICT;

CREATE TABLE collections (
  id INTEGER PRIMARY KEY, uid TEXT NOT NULL UNIQUE,
  slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL
) STRICT;

CREATE TABLE document_collections (
  document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
  PRIMARY KEY (document_id, collection_id)
) STRICT;
```

**Approvals attach to a version, not a document.** Edit the document, a new
version exists, and the old approvals no longer satisfy the guard. Getting
"changes invalidate sign-off" right costs one foreign key. The `UNIQUE`
constraint makes "two *distinct* people must approve" a plain `COUNT(*)`.

Comments are a thread. Bricolage's entire collaboration story was one
overwritten note per version. Keep `document_versions.note` as well — the
check-in message is a different thing from a discussion.

## 6. The workflow engine

`internal/workflow`.

### 6.1 Guards are a closed vocabulary

```go
type Guard string

const (
    GuardNoteRequired     Guard = "note_required"
    GuardAssigneeOnly     Guard = "assignee_only"
    GuardApprovalsMet     Guard = "approvals_met"
    GuardNotLocked        Guard = "not_locked"
    GuardCommentsResolved Guard = "comments_resolved"
    GuardHasSlug          Guard = "has_slug"
    GuardHasCoverDate     Guard = "has_cover_date"
)
```

```go
type Effect string

const (
    EffectClearAssignee  Effect = "clear_assignee"
    EffectAssignToActor  Effect = "assign_to_actor"
    EffectClearApprovals Effect = "clear_approvals"
    EffectSetDueIn       Effect = "set_due_in"     // parameterised: "48h"
)
```

Transitions are data so an editorial process can be configured. Guards are a
closed set so that every guard which can be configured is one the engine
actually enforces.

> **The rule this exists to prevent:** Bricolage shipped `desk.pre_chk_rules`
> and `desk.post_chk_rules` — columns with foreign keys, indexes, an accessor
> API, and a comment reading `# Do the pre-desk rule checks` above a call that
> checked nothing. Nothing in the entire tree ever read them. A rule column that
> nothing implements is worse than no column, because it lies to whoever reads
> the schema.
>
> **Never merge a guard name without its enforcement, in the same commit.**

Deliberately absent: a scripting language. Bricolage evaluated alert rules
inside a `Safe` sandbox. That is a security surface for something a `switch`
statement handles.

### 6.2 Availability and execution share one function

```go
type Allowed struct {
    Transition Transition
    Reason     string // why not, when Permitted is false
    Permitted  bool
}

// Available returns every transition out of doc's current state, each marked
// with whether actor may perform it and why not. The UI renders from this.
func (e *Engine) Available(ctx context.Context, doc *domain.Document, actor *domain.User) ([]Allowed, error)

// Do performs one transition. It re-runs the identical checks.
func (e *Engine) Do(ctx context.Context, req Request) (*domain.Document, error)
```

Both call the same unexported `check`. This is not a style preference. It is the
structural fix for the defect described in §3, and it makes "the UI offered
something the server refuses" unrepresentable.

`Available` returning *refused* transitions with reasons is deliberate: the UI
can grey out "Approve" and say "needs one more approval", which is far better
than the action silently not existing.

### 6.3 Transitions are the only writer of `documents.state`

No other code path assigns to that column. Not `store`, not a service method,
not a migration fix-up. If something needs to move a document, it calls
`workflow.Engine.Do`. Enforce this by keeping the state-writing statement inside
`internal/workflow` and giving `store` no exported method that sets state alone.

### 6.4 Transactionality

`Do` runs inside one SQLite transaction:

1. load document and transition, `check`
2. update `documents` (state, effects)
3. insert the `events` row
4. enqueue any jobs

Alert evaluation happens **after commit**, driven off the event row, so a
failing notification cannot roll back an editorial action.

## 7. Authorization

`internal/authz`. Roles plus **scoped grants**.

```sql
CREATE TABLE roles (
  id INTEGER PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL
) STRICT;

CREATE TABLE user_roles (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, role_id)
) STRICT;

CREATE TABLE grants (
  id            INTEGER PRIMARY KEY,
  role_id       INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  privilege     INTEGER NOT NULL,           -- 1 read .. 5 publish, 255 deny

  -- Scope: NULL is a wildcard. A grant matches when every non-NULL column matches.
  site_id       INTEGER REFERENCES sites(id),
  doc_kind      TEXT,
  category_id   INTEGER REFERENCES categories(id),
  category_deep INTEGER NOT NULL DEFAULT 1,
  workflow_id   INTEGER REFERENCES workflows(id),
  state         TEXT,
  collection_id INTEGER REFERENCES collections(id),
  document_id   INTEGER REFERENCES documents(id),

  created_at    TEXT NOT NULL,
  created_by    INTEGER REFERENCES users(id)
) STRICT;
```

Privilege scale, kept from Bricolage because it is well chosen:

| Value | Name | Meaning |
|---|---|---|
| 1 | `READ` | view |
| 2 | `EDIT` | modify |
| 3 | `RECALL` | pull back out of a terminal state |
| 4 | `CREATE` | create new |
| 5 | `PUBLISH` | publish |
| 255 | `DENY` | veto |

Two properties to preserve exactly:

- **Ordered and cumulative**, so "may they do this?" is one integer comparison.
- **`DENY = 255` is the numeric maximum**, so effective privilege is
  `MAX(privilege)` over matching grants and deny-overrides falls out for free.
  No second pass, no special case.

`RECALL` is worth keeping as a distinct level: it is the one genuinely
workflow-shaped privilege and generic CRUD permission sets have no equivalent.

### 7.1 Why not groups

Bricolage granted permission from a *group of users* to a *group of objects*,
and computed a document's groups at query time as the union of its categories,
its workflow, its desk, and its kind. That made permissions **follow the
document**: file a story under `/features` and the features grants apply, with
no ACL written anywhere. That property is worth keeping and plain role-based
access control cannot express it.

But the groups were a disguise. Category "asset groups" had no member rows at
all — `Category.pm` creates one per category with a comment beginning
`XXX Yes, it's ugly`, and documents only pretend to belong. What the group ids
actually encoded were *attributes of the document*: kind, category, workflow,
state. Scoped grants keep the attributes and drop the disguise.

This also gains two things Bricolage could not do:

- **Category subtree inheritance.** There is no ancestor walk anywhere in the
  Perl authorization path, so a grant on `/features` did not cover
  `/features/film`. `category_deep` fixes that with a `path` prefix match.
- **Per-document grants.** `document_id` is one row. Bricolage's documented
  workaround was creating a one-member user group *and* a one-member object
  group.

Two capabilities are genuinely given up: nested groups (roles are flat — use
multiple role assignment; add `parent_role_id` only when a concrete need
appears), and uniform authorization of non-document objects (admin objects are
scoped by role and privilege alone).

### 7.2 Resolution

```go
func Resolve(grants []Grant, subj Subject) Privilege // MAX, DENY wins
```

Load a user's grants once per request, cache on the request context, evaluate in
Go. The equivalent SQL is the definition, not the hot path.

### 7.3 Anti-escalation

**Nobody may create a grant conferring a privilege they do not themselves hold
over that scope.** Bricolage learned this in 1.8.0 after discovering that a
System Admin could add themselves to Global Admins. Implement it in the same
function that writes a grant, from the first commit, and test it.

## 8. Publishing

`internal/publish`. This is the part of Bricolage that was genuinely excellent
and the reason the system had its reputation.

### 8.1 Scheduled publishes are pinned to a version

A publish job's payload names a `document_version_id`, never a `document_id`.

An editor approves version 5 for midnight and keeps working; at midnight,
version 5 publishes — not whatever the draft has become. The immutability
trigger (§5.1) is what makes that promise real. A design that models this as
"publish the current draft at time T" cannot express it, and it is the property
editorial teams actually depend on.

### 8.2 Related-asset cascade

Publishing a document publishes the documents it references. The traversal must
be:

- **cycle-safe** — a `seen` set keyed by document id, with related documents
  pushed onto the work queue so traversal is recursive
- **permission-checked per node** — `PUBLISH` on each related document, not just
  the root
- **state-gated** — a related document that is in a workflow must be in a
  `publishable` state, otherwise it is refused by name
- **lock-gated** — a checked-out relative is refused by name
- **reviewable** — the gathered set is returned to the caller for confirmation
  before anything is scheduled
- **policy-driven** — `publish.related_failure` is `fail` or `warn`

Every one of those bullets is a lesson someone learned in production. Implement
the traversal as a pure function over a loaded graph so it can be tested without
a database.

### 8.3 Resources and stale expiry

```sql
CREATE TABLE published_resources (
  id                INTEGER PRIMARY KEY,
  document_id       INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  output_channel_id INTEGER NOT NULL REFERENCES output_channels(id),
  version_id        INTEGER NOT NULL REFERENCES document_versions(id),
  uri               TEXT    NOT NULL,
  path              TEXT    NOT NULL,
  checksum          TEXT    NOT NULL,
  bytes             INTEGER NOT NULL,
  published_at      TEXT    NOT NULL,
  UNIQUE (output_channel_id, uri)
) STRICT;
```

Remember every file written. On republish, diff the new URI set against the
stored set and schedule a delete job for every URI no longer produced:

```sql
DELETE FROM published_resources
 WHERE document_id = :doc AND output_channel_id = :oc
   AND uri NOT IN (SELECT value FROM json_each(:new_uris))
RETURNING uri, path;
```

Without this, changing a cover date, category, or slug leaves the file at the
old URI serving forever. Almost no CMS gets this right. **Build it with
publishing, not after** — retrofitting means a reconciliation pass over an
already-dirty output tree.

`UNIQUE (output_channel_id, uri)` also gives URI-collision detection for free.
Detect it by checking for `SQLITE_CONSTRAINT_UNIQUE` on the result code, **never
by matching error text**. Bricolage detected collisions by regexing PostgreSQL
7.1–7.4 error strings, which is why its friendly "URI is not unique" message has
not fired on any server built this century.

### 8.4 Rendering

`internal/render`. `html/template`, with template lookup walking up the category
tree: a document in `/features/film/` looks for its element type's template in
`/features/film/`, then `/features/`, then `/`. First match wins. This is the
one piece of Bricolage's templating worth keeping and it is about fifteen lines.

Three modes, as in the original: **publish** (write to the output tree),
**preview** (write to a scratch tree, serve back), **validate** (parse and
type-check, write nothing).

Media binaries are content-addressed on disk (`blobs/<sha256[:2]>/<sha256>`)
with metadata in the database. The database stores no blobs.

## 9. Jobs

`internal/jobs`.

```sql
CREATE TABLE jobs (
  id               INTEGER PRIMARY KEY,
  kind             TEXT    NOT NULL,
  priority         INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
  scheduled_for    TEXT    NOT NULL,
  payload          TEXT    NOT NULL,
  lease_owner      TEXT,
  lease_expires_at TEXT,
  attempts         INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 5,
  last_error       TEXT,
  completed_at     TEXT,
  failed_at        TEXT,
  created_by       INTEGER REFERENCES users(id),
  created_at       TEXT    NOT NULL
) STRICT;

CREATE INDEX jobs_claimable ON jobs(scheduled_for, priority)
  WHERE completed_at IS NULL AND failed_at IS NULL;
```

Claiming is one statement:

```sql
UPDATE jobs
   SET lease_owner = :worker, lease_expires_at = :deadline, attempts = attempts + 1
 WHERE id = (
       SELECT id FROM jobs
        WHERE completed_at IS NULL AND failed_at IS NULL
          AND scheduled_for <= :now
          AND (lease_expires_at IS NULL OR lease_expires_at < :now)
        ORDER BY priority ASC, scheduled_for ASC, id ASC
        LIMIT 1)
RETURNING id, kind, payload, attempts, max_attempts;
```

This is Bricolage's compare-and-swap with its bug fixed. Its lock had no lease
and no heartbeat, so a worker that died mid-job left the row marked executing
forever and recovery was a manual `UPDATE`. A lease expires on its own.

**Priority is load-bearing, not decoration.** It is what lets a fifty-thousand
document republish run at priority 5 without blocking an editor pressing
Publish. Every bulk operation defaults to priority 5.

Workers run inside `cmsd` by default (`--workers N`, default 1, `0` to disable).
Long jobs must heartbeat by extending their lease.

## 10. Events and notifications

`internal/events`.

```sql
CREATE TABLE events (
  id           INTEGER PRIMARY KEY,
  type         TEXT    NOT NULL,
  actor_id     INTEGER          REFERENCES users(id),  -- NULL = system
  subject_kind TEXT    NOT NULL,
  subject_id   INTEGER NOT NULL,
  payload      TEXT    NOT NULL DEFAULT '{}',
  occurred_at  TEXT    NOT NULL
) STRICT;

CREATE INDEX events_subject ON events(subject_kind, subject_id, id DESC);
CREATE INDEX events_type    ON events(type, occurred_at);
```

Bricolage used four tables and a seeded registry of 153 rows. One table with a
JSON payload does the same work. Event types are Go constants with display names
in a registry; the admin UI lists them from code, not from a `SELECT`.

**Every state change writes an event, and the payload carries enough to
reconstruct what happened.** A document's history is then a query, not a log
grep. Wire this from the first milestone that changes state — retrofitting an
audit log is miserable.

Alerts are a rule engine over the event stream:

```sql
CREATE TABLE alert_rules (
  id INTEGER PRIMARY KEY, uid TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL, event_type TEXT NOT NULL,
  conditions TEXT NOT NULL DEFAULT '[]',   -- JSON [{field, op, value}]
  channel TEXT NOT NULL, target TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1
) STRICT;

CREATE TABLE notifications (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  rule_id INTEGER REFERENCES alert_rules(id) ON DELETE SET NULL,
  read_at TEXT, created_at TEXT NOT NULL
) STRICT;
```

Conditions resolve field values from three sources in order — the acting user,
the event payload, then the subject — which is the clever part of Bricolage's
design and worth copying. Operators: `eq ne lt lte gt gte in contains matches`.
`matches` is a Go regexp, compiled once and cached; reject rules whose pattern
does not compile at save time, not at fire time.

## 11. The three commands

### `cmsdb` — database lifecycle

```
cmsdb init            --db PATH               create the file, apply all migrations
cmsdb migrate status  --db PATH               show applied/pending
cmsdb migrate up      --db PATH [--to N]      apply pending migrations
cmsdb bootstrap admin --db PATH --email E --name N [--password-stdin]
cmsdb seed            --db PATH [--demo]      default workflow, roles, site, element types
cmsdb check           --db PATH               integrity: FK check, orphaned resources, stuck leases
cmsdb vacuum          --db PATH
```

`bootstrap admin` reads a password from stdin, or generates one and prints it
**once** to stdout. It never accepts a password as a command-line flag —
arguments are visible in `ps` and land in shell history. It is idempotent: a
second run with the same email updates nothing and exits non-zero with a clear
message.

`seed` is separate from `init` so that tests can create an empty schema.

### `cmsd` — the server

```
cmsd serve --db PATH [--addr 127.0.0.1:18443] [--workers N] [--config FILE]
           [--env development|production] [--timeout DURATION]
cmsd routes                       print the route table and exit
cmsd version                      includes whether dev routes are compiled in
```

`--env` defaults to `production` (§14). `--timeout` shuts down gracefully after
the given duration; `0`, the default, means never. The `/__development/*` routes
require `--env development` **and** a binary built with `-tags dev`; see
"Development affordances" below.

Serves three things from one process:

- `/api/v1/...` — the JSON REST API (§12)
- `/...` — the HTMX UI, `html/template` rendered
- background job workers, unless `--workers 0`

Graceful shutdown: stop accepting, drain in-flight requests, let workers finish
the current job or release its lease, close the pool.

### Serving model: `cmsd` runs behind a reverse proxy

**`cmsd` speaks plain HTTP on loopback and never terminates TLS.** A reverse
proxy — Caddy or nginx — terminates it and guarantees TLS 1.3 or better. That
is true in production and simulated in development, so there is one serving
model, not two.

| | Public URL | `cmsd` listens on |
|---|---|---|
| Development | `https://htmx-app.localhost:8443/` | `127.0.0.1:18443` |
| Production | the site's real origin | loopback, port from config |

`deploy/Caddyfile.dev` in this repository is the development proxy. Caddy
issues a local certificate for `*.localhost` automatically, so there is nothing
to install.

`--addr` **defaults to a loopback address, never `:8080` or `0.0.0.0`.** Binding
a TLS-less server to a public interface is the kind of mistake that survives to
production. If someone genuinely needs it, they can pass it explicitly.

Four consequences that are easy to get wrong, and are therefore requirements:

**1. The public origin is configuration, not inference.** `cmsd` is told its
external origin (`server.public_origin`, e.g.
`https://htmx-app.localhost:8443`). It never derives it from the `Host` header
alone. Absolute URLs, cookie domains, and the origin allowlist all come from
this value.

**2. Session cookies are `Secure` even though the local connection is not.**
The flag describes the *browser's* connection, which is TLS, not the proxy hop.
Set `Secure`, `HttpOnly`, and `SameSite=Lax` unconditionally. Do not make
`Secure` conditional on `r.TLS != nil` — that check is always false here and the
resulting code silently ships insecure cookies.

**3. Trust forwarded headers only from the proxy.** `X-Forwarded-For`,
`X-Forwarded-Proto` and `X-Forwarded-Host` are attacker-controlled unless the
connection came from a configured trusted address. Keep a `server.trusted_proxies`
CIDR list, default `127.0.0.1/32` and `::1/128`, and ignore the headers from any
other peer. The client IP written to the event log and the rate limiter must be
the resolved one.

**4. CSRF uses `net/http.CrossOriginProtection`,** which reads `Sec-Fetch-Site`
and `Origin`. Register the public origin as trusted and confirm the proxy passes
`Origin` through unmodified — Caddy does by default. Protection wraps the HTML
UI and any cookie-authenticated route. Bearer-token API routes do not need it,
because a token is not sent ambiguously by a browser; a request carrying a
bearer token is exempt, one carrying a session cookie is not.

The proxy owns TLS configuration, HSTS, HTTP→HTTPS redirection, and certificate
lifecycle. `cmsd` owns none of them and must not emit HSTS headers of its own.

### Development affordances

Three concessions exist so that an automated agent can drive the system without
a human at a keyboard. They are a deliberate, documented trade, not an
oversight.

The problem they solve: an agent cannot type a password into a prompt, cannot
reliably stop a server it started in the background, and leaves orphaned
processes behind when a session ends badly. Without these, every agent session
accretes dead `cmsd` processes holding SQLite locks.

| Affordance | Form | Gated |
|---|---|---|
| Log in as an existing user, no password | `GET /__development/log-me-in/{email}?returnTo={url}` | yes |
| Stop the server over HTTP | `GET` or `POST /__development/shut-it-down` | yes |
| Stop the server after a duration | `cmsd serve --timeout 30m` | **no** |

`--timeout` is not gated because it is not dangerous. It ships in every build.
The two routes are gated, because either one is a complete authentication
bypass if it reaches production.

#### The guard stack

Four independent things must all go wrong before these routes are reachable by
someone who should not have them.

**1. Build tag.** The code is physically absent from a default build.

```go
// internal/web/devroutes/routes.go
//go:build dev

package devroutes

const CompiledIn = true

func Register(mux *http.ServeMux, d Deps) { /* the real routes */ }
```

```go
// internal/web/devroutes/stub.go
//go:build !dev

package devroutes

const CompiledIn = false

func Register(mux *http.ServeMux, d Deps) {}
```

`go build ./cmd/cmsd` produces a binary in which these handlers do not exist.
`go build -tags dev ./cmd/cmsd` produces one in which they do. There is no
runtime path from the first to the second.

**2. The environment is `development`.** Even in a `dev` build, the routes
register only when the running environment is exactly `development` (§14). The
default is `production`, so this must be opted into explicitly and a production
run script will not have done so. Shipping a `dev`-tagged binary is therefore
not by itself enough to expose them.

There is deliberately **no separate `--dev` flag**. Two switches meaning almost
the same thing is how you end up with both of them set in production by
somebody trying to make an error go away. The environment is the switch.

**3. Loopback peer.** Each handler refuses any request whose *peer* address —
the real TCP peer, never `X-Forwarded-For` — is not loopback. Behind the proxy
the peer is always loopback, so this does not help there; it exists to stop the
routes answering a direct connection from another machine.

**4. It is loud.** `cmsd serve` prints its environment on every start, in every
environment, so `environment=production` is greppable in a production log. When
the dev routes are actually live it also prints:

```
*** ENVIRONMENT=development: /__development/* routes are enabled.
*** Anyone who can reach this listener can log in as any user.
```

Every request that reaches a `/__development/*` handler logs at `WARN` with the
path, the email where relevant, and the resolved peer.

`cmsd version` reports `dev-routes: compiled-in` or `absent`, so an incident
responder can tell what is running without reading the build command.

#### The truth table

| Build | `environment` | `/__development/*` |
|---|---|---|
| default | `production` | 404 |
| default | `development` | 404 — code is not in the binary |
| `-tags dev` | `production` | 404 — not registered |
| `-tags dev` | `development` | **live** |

Only the last row exposes them, and reaching it takes a deliberate build *and* a
deliberate configuration.

#### `GET /__development/log-me-in/{email}?returnTo={url}`

Creates a session for an **existing** user, without a password.

- It does **not** create accounts. An unknown email is a 404. This keeps the
  blast radius to accounts that already exist and matches what agents need:
  `cmsdb bootstrap admin`, then log in as that admin.
- It writes a `session.dev_login` event with the email and the peer address.
  Invariant: every state change writes an event, and this is a state change.
  It is also exactly the line you want in the log during an incident.
- Response depends on the request:
  - `returnTo` present → `302` to it, session cookie set
  - `Accept: application/json` → `200` with `{"token":"…","user":{…}}`
  - otherwise → `200` with a plain-text token
- **`returnTo` is validated**: it must be a relative path, or absolute with an
  origin equal to `server.public_origin`. Anything else is a 400. An open
  redirect in a dev-only route is still an open redirect, and the pattern gets
  copied.
- Sessions it creates are ordinary sessions with the ordinary expiry. It grants
  no extra privilege — the user's roles and grants apply exactly as they would
  after a real login.

#### `/__development/shut-it-down`

Graceful shutdown, same path as `SIGTERM`: stop accepting, drain in-flight
requests, let workers finish the current job or release its lease, close the
pool, exit 0.

Both `GET` and `POST` are accepted. `GET` is a footgun in general — a link
prefetch can trigger it — but nothing links to this route, and `curl` without
`-X POST` is what an agent will reach for. Accepting both is the right trade
here; do not copy the pattern elsewhere.

The handler **writes its response and flushes before beginning shutdown**, so
the caller gets a `200` rather than a connection reset. Trigger the shutdown
from a goroutine after the flush.

#### `--timeout <duration>`

```
cmsd serve --timeout 30m
```

After the duration, shut down gracefully through the same path. Default `0`,
meaning no timeout. Exit code `0` — an expired timeout is a normal shutdown,
not a failure. Log one line at `INFO` when it fires, naming the configured
duration.

This is ungated and available in production builds. It is genuinely useful in
CI and in short-lived containers, and it cannot be abused: the worst it does is
stop a server that its own operator configured to stop.

#### The rule

**Release artifacts are built without `-tags dev`, and CI proves it.** A test in
the default build asserts that `/__development/log-me-in/anyone@example.com` and
`/__development/shut-it-down` both return `404`. If that test ever fails, the
build is not shippable.

### `earl` — the API client

Named for what it does: it speaks to URLs. It is a first-class client, not a
debug tool, and **it is the acceptance-test harness for every milestone.**

```
earl login   --server URL --email E                 prompts for password, stores token
earl login   --server URL --email E --dev           no password; server must be in development (§11)
earl whoami
earl doc list        [--state S] [--assignee U|--unassigned] [--overdue] [--site S]
earl doc show        UID
earl doc create      --element-type T --title X [--category C]
earl doc edit        UID                            $EDITOR on the content JSON
earl doc checkout    UID
earl doc checkin     UID [--note N]
earl doc revert      UID
earl doc diff        UID --from N --to M
earl doc transitions UID                            what may I do, and why not
earl doc do          UID TRANSITION [--note N]
earl doc assign      UID --to USER [--due WHEN]
earl doc approve     UID
earl doc comment     UID [--reply-to ID] BODY
earl doc events      UID
earl publish         UID [--at TIME] [--channel C]... [--dry-run]
earl queue           SLUG
earl job list        [--failed] [--pending]
earl job retry       ID
earl admin grant     --role R --privilege P [scope flags...]
```

Every command supports `--json` for machine-readable output; the default is a
human-readable table. Configuration from `--server`/`$EARL_SERVER` and a token
in `~/.config/earl/credentials.json` at mode `0600`.

`earl publish --dry-run` returns the related-asset set that *would* be
published, with each refusal and its reason. That is the CLI face of §8.2.

## 12. HTTP API

`/api/v1`, JSON, resource-oriented. Authentication is a bearer token; the UI
uses a cookie session plus CSRF protection.

```
POST   /api/v1/sessions                          log in, return token
DELETE /api/v1/sessions/current                  log out
GET    /api/v1/me

GET    /api/v1/documents                         list; filters as query params
POST   /api/v1/documents                         create
GET    /api/v1/documents/{uid}
PATCH  /api/v1/documents/{uid}                   metadata: title, slug, categories, due
POST   /api/v1/documents/{uid}/checkout
POST   /api/v1/documents/{uid}/checkin
POST   /api/v1/documents/{uid}/revert
GET    /api/v1/documents/{uid}/versions
GET    /api/v1/documents/{uid}/versions/{n}
GET    /api/v1/documents/{uid}/diff?from=&to=

GET    /api/v1/documents/{uid}/transitions       available, with refusal reasons
POST   /api/v1/documents/{uid}/transitions       {"to":"review","note":"..."}

POST   /api/v1/documents/{uid}/assignment        {"user":"...","due_at":"..."}
DELETE /api/v1/documents/{uid}/assignment
POST   /api/v1/documents/{uid}/approvals
DELETE /api/v1/documents/{uid}/approvals/current
GET    /api/v1/documents/{uid}/comments
POST   /api/v1/documents/{uid}/comments
POST   /api/v1/comments/{uid}/resolution
GET    /api/v1/documents/{uid}/events

POST   /api/v1/documents/{uid}/publications      {"at":...,"channels":[...],"dry_run":bool}
GET    /api/v1/documents/{uid}/resources

GET    /api/v1/queues/{slug}
GET    /api/v1/jobs
POST   /api/v1/jobs/{id}/retry
GET    /api/v1/notifications
POST   /api/v1/notifications/{id}/read

GET/POST/PATCH  /api/v1/sites, /categories, /output-channels,
                /element-types, /workflows, /roles, /grants, /users
```

Modelling **transitions as a subresource** is the point of the design: `GET`
tells you what the state machine permits and why, `POST` performs one. It makes
the workflow visible in the API instead of hiding it behind `PATCH state=`.
Never expose a plain `PATCH` that sets `state`.

Errors are RFC 9457 problem documents:

```json
{ "type": "https://.../errors/guard-failed",
  "title": "Transition refused",
  "status": 409,
  "detail": "not enough approvals: 1 of 2",
  "guard": "approvals_met" }
```

| Situation | Status |
|---|---|
| unauthenticated | 401 |
| authenticated, privilege insufficient | 403 |
| unknown uid | 404 |
| guard refused / lock held / state conflict | 409 |
| malformed body, invalid content | 422 |

Requests carry `Idempotency-Key` on `POST`s that create jobs; store the key with
the created resource and return the same result on replay.

## 13. Persistence rules

These are not suggestions. SQLite punishes casual concurrency.

- **WAL mode**, `foreign_keys = ON`, `busy_timeout` set on every connection.
- **One writer.** Use a `sqlitex.Pool` for readers and a **single** dedicated
  write connection, serialized. Do not let N goroutines open write transactions
  and hope `busy_timeout` sorts it out.
- **`STRICT` tables everywhere.** No affinity surprises.
- **Timestamps are ISO-8601 UTC `TEXT`**, `2006-01-02T15:04:05.000Z`. Sortable,
  comparable in SQL, readable in a shell. Never store local time.
- **Booleans are `INTEGER` 0/1.** SQLite has no boolean type.
- **All SQL lives in `internal/store`.** No SQL string anywhere else, ever.
- **No ORM, no query builder.** Hand-written SQL, named parameters.
- **Every migration is append-only.** Once a migration file is committed and
  released, it is never edited. Fix mistakes with a new migration.
- **`store` methods take an explicit transaction handle** so that `service` can
  compose several into one transaction.
- Internal identifiers are `INTEGER PRIMARY KEY`. External identifiers are the
  `uid` column, a lowercase ULID. **The API speaks only `uid`.** Integer ids
  never appear in a URL, a JSON body, or a log line intended for users.

## 14. Cross-cutting

**Environment.** One setting, two values, governing everything that should
differ between a developer's machine and a real deployment.

```
environment  development | production        default: production
```

Sources, in precedence order: `--env`, `$CMS_ENV`, the config file, then the
default. **The default is `production`**, which is the fail-safe direction: an
unset or misspelled value never accidentally unlocks anything. Never infer the
environment from a hostname, a listen address, or whether a terminal is
attached.

It governs:

| | `development` | `production` |
|---|---|---|
| `/__development/*` routes | live, if built `-tags dev` (§11) | never |
| Log format | console, human-readable | JSON |
| Templates | re-read from disk per request | parsed once at startup |
| Error responses | include the underlying detail | generic, with a request id |
| Startup banner | prints the loud warning | prints `environment=production` |

`production` is also the right value for staging and for CI. A third value was
considered and rejected: more states mean more combinations nobody tests, and
anything that is not a developer's laptop should behave like production.

`cmsd serve` logs its environment on every start, in both environments, so the
value is greppable in a log rather than inferred from a run script.

**Time.** `internal/clock` defines `type Clock interface { Now() time.Time }`.
Every component that needs time takes one. `time.Now()` appears in `main` and in
the real clock implementation, nowhere else. This is what makes lease expiry,
scheduling, and due dates testable.

**Errors.** Sentinel errors in `domain` (`ErrNotFound`, `ErrConflict`,
`ErrForbidden`, `ErrGuardFailed`), wrapped with `%w`, inspected with
`errors.Is`/`errors.As`. Mapping to HTTP status happens **only** at the
transport edge, in one function.

**Logging.** `log/slog`, structured, JSON in production. Every request gets a
request id, propagated in context and returned in a response header. Never log
tokens, password hashes, or full document content.

**Configuration.** Flags override environment variables, environment variables
override the file, the file overrides defaults. One `config.Config` struct.
Secrets come from environment variables or the file, never from flags.

```
environment             production             see "Environment" above

server.addr             127.0.0.1:18443        loopback only; never 0.0.0.0
server.public_origin    https://htmx-app.localhost:8443
server.trusted_proxies  ["127.0.0.1/32", "::1/128"]
server.workers          1
server.timeout          0                      graceful shutdown after this; 0 = never
```

`public_origin` is authoritative for absolute URLs, cookie attributes, and the
CSRF trusted-origin list. It is never inferred from the `Host` header.

**Passwords.** `golang.org/x/crypto/bcrypt` or argon2id. Session tokens are 32
random bytes, base64url; the database stores only a SHA-256 of the token.

**Cookies.** `Secure`, `HttpOnly`, `SameSite=Lax`, unconditionally. The `Secure`
flag describes the browser's connection, which is TLS, not the loopback hop from
the proxy. **Never gate it on `r.TLS != nil`** — that expression is always false
behind the proxy, and the resulting code ships insecure cookies while looking
careful. See §11.

**Client addresses.** Resolve the real client IP from `X-Forwarded-For` only
when the peer is in `server.trusted_proxies`; otherwise use the peer address.
Event log entries and any rate limiting use the resolved value. Do a single
resolution in one middleware and put the result on the context; never parse the
header twice.

**Context.** Every `service`, `store`, and `jobs` method takes
`context.Context` first and honours cancellation.

## 15. Testing

- **`domain` is unit tested exhaustively.** It is pure; there is no excuse.
  Table-driven, including every guard and every URI format case.
- **`store` is tested against a real SQLite database**, in-memory, with all
  migrations applied by the same code path `cmsdb` uses. Not a mock.
- **`service` is tested through its public methods** with a real store and a
  fake clock. Assert on emitted events as well as returned values — an operation
  that does not write its event is not finished.
- **`api` is tested with `httptest`**, asserting status, problem type, and body
  shape.
- **Golden files** for rendering and for `earl --json` output. Regenerate with
  `go test ./... -update`.
- **One end-to-end test per milestone**, driving `earl` against a `cmsd` on a
  temporary database. This is the milestone's acceptance criterion made
  executable.

Concurrency tests that matter, because these are where the original failed:

- two workers race for one job; exactly one claims it
- a worker dies holding a lease; another claims the job after expiry, and
  `attempts` is correct
- two users check out the same document; exactly one succeeds
- a publish scheduled against version 5 publishes version 5 after version 6 is
  checked in

## 16. Decisions deferred

Named here so they do not arrive by accident.

- **No parallel or branching workflow.** One `state` column, not a Petri net. If
  legal and copy must both see a document, that is two sequential states or two
  approvals in one state.
- **No cross-workflow transfer.** Bricolage's flag for this made an already
  fully-connected desk graph more connected. A document belongs to one workflow.
- **No workflow versioning.** Editing a workflow affects documents already in
  it. Guard the sharp edge — refuse to delete a state that documents occupy —
  and leave it.
- **No nested roles.** Flat, with multiple assignment.
- **No per-field permissions.**
- **No multi-node deployment.** One process, one SQLite file. Revisit only with
  a concrete requirement.
- **Project naming.** The module stays `github.com/mdhender/bricolage` for the
  duration of the build, even though the binaries are `cmsdb`/`cmsd`/`earl` and
  this is no longer a Bricolage port. A rename may happen once the work is
  finished. Until then: do not rename the module, do not rename the repository,
  and do not introduce a second name for the project in code or documentation.
