# Deployment

`cmsd` speaks plain HTTP on loopback and never terminates TLS. A reverse proxy
terminates it and guarantees TLS 1.3 or better. This is true in production and
simulated in development, so there is one serving model rather than two.

See `docs/DESIGN.md` §11, "Serving model", for the four requirements this
imposes on the application: the public origin is configuration rather than
inference, session cookies are `Secure` regardless of the local scheme,
forwarded headers are trusted only from configured proxy addresses, and CSRF
protection is `net/http.CrossOriginProtection` with the public origin registered
as trusted.

## Development

**Caddy runs as a machine-wide Homebrew service. Never run it yourself** — not
`caddy run`, not `caddy start`, not `caddy trust`, and above all not
`caddy run --config deploy/Caddyfile.dev`. `Caddyfile.dev` in this directory is
an **EXAMPLE ONLY**: it documents the shape of the proxy and is not the
configuration this machine serves.

Running `caddy` as your own user account mints a second internal CA root with
the same subject name as the service's and trusts it. Two same-subject anchors
make chain building non-deterministic, so HTTPS to `*.localhost:8443` starts
failing at random and clearing it needs `sudo` keychain surgery.

The service reads `/opt/homebrew/etc/Caddyfile`, which already terminates TLS
for `https://htmx-app.localhost:8443` and proxies it to `127.0.0.1:18443`. Its
CA lives in `/opt/homebrew/var/lib/caddy/pki/`. Start only `cmsd`:

```sh
brew services list | grep caddy    # expect "started"; if not, ask a human
mkdir -p var                       # no command creates a directory; this one is yours
go run ./cmd/cmsdb init --db ./var                 # creates ./var/cms.db
go run ./cmd/cmsd serve --db ./var --addr 127.0.0.1:18443 --env development --timeout 60m
```

`--db` names an existing directory; the database inside it is always `cms.db`.
`cmsd` neither creates nor migrates it, so a fresh checkout needs the `cmsdb
init` line above once.

Open <https://htmx-app.localhost:8443/>.

When diagnosing local TLS, inspect the chain actually served on the wire and
test with `/usr/bin/curl`; Homebrew's `openssl` and `curl` use their own CA
bundles and will disagree with the macOS keychain.

`--env development` enables the `/__development/*` routes, which let an
automated agent log in without a password and stop the server over HTTP. There
is no tag involved: the environment is the only switch, so `go run ./cmd/cmsd`
works without a build step. In any other environment the routes are never
registered. (The `production` tag under "Building for the server" is a
placement check for release binaries; it gates no routes.) `--timeout` is
ungated and useful anywhere; always pass one in development so an abandoned
server does not sit on the SQLite lock.

`earl` points at the same public URL, not at the Go listener:

```sh
go run ./cmd/earl login --server https://htmx-app.localhost:8443
```

Talking to `http://127.0.0.1:18443` directly bypasses the proxy and therefore
exercises a code path that does not exist in production. Do it only when
debugging the proxy itself.

## Production

Any proxy that terminates TLS 1.3+, sets the standard forwarded headers, and
passes `Origin` and `Sec-Fetch-*` through unmodified will do. The proxy owns
TLS configuration, HSTS, HTTP→HTTPS redirection, and certificates. `cmsd` owns
none of them.

`deploy/Caddyfile.prod` and `deploy/nginx.conf` are worked examples of each,
for `cms.mdhenderson.com`. `deploy/PROVISIONING.md` is the first-time setup of
the droplet they run on; `deploy/cms.service` is the unit.

Everything `cmsd` needs is a flag, plus one environment variable. **There is no
configuration file.** `docs/DESIGN.md` §11 lists a `--config FILE`, and
`cmd/cmsd/main.go` says in a comment where it would be read; it is not
implemented, and a flag that is parsed and ignored is worse than no flag. Until
it exists, the unit file's `ExecStart` is the configuration:

| Flag | Value |
|---|---|
| `--addr` | a loopback address and port (default `127.0.0.1:18443`) |
| `--public-origin` | the browser-facing origin, scheme included |
| `--trusted-proxy` | CIDRs the proxy connects from (default `127.0.0.1/32`, `::1/128` — already right when the proxy is on the same host) |
| `--timeout` | optional graceful shutdown after a duration; `0`, the default, means never |
| `--db` | the directory holding `cms.db`; it must already exist |
| `--templates`, `--preview`, `--output` | optional directories that must already exist; without them the server renders, previews, and publishes nothing |
| `--workers` | background job workers in this process; `0` disables them |
| `$CMS_ENV` | `production` — see below |

`CMS_ENV=development` on a server is a complete authentication bypass: it is
what registers the `/__development/*` routes, and either of them logs anyone in
as anyone. The environment is the only gate on those routes, so this one
variable carries the whole weight. Set it in the unit file. Never set it in an
interactive shell profile, and never copy a development `.env` onto a server.

CI asserts that a server started with no `--env` and no `CMS_ENV` returns 404
for every `/__development/*` route, and that the same routes are live under
`--env development`; those assertions gate release.

## Building for the server

Release binaries are built with `-tags production`. The tag adds one thing: a
`buildenv.Verify()` that each `main` calls at startup, which **panics unless
`CMS_ENV` is exported as exactly `production`**. Binaries built without the tag
have the mirror check — they panic if `CMS_ENV` *is* `production`. A binary
therefore cannot run on the wrong kind of machine without saying so
immediately, in the logs, at startup, rather than quietly serving the wrong
configuration. See `docs/DESIGN.md` §14, "The build/environment interlock".

The tag gates **nothing else**. It adds no routes, removes no code, and changes
no behaviour beyond that one assertion.

`make release` is this loop, and is the one place in the repository that runs
`mkdir` — build output, not data:

```sh
mkdir -p deploy/linux/amd64
for c in cmsd cmsdb earl; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -tags production -trimpath -o deploy/linux/amd64/$c ./cmd/$c
done
```

`CGO_ENABLED=0` is correct here rather than merely convenient: the SQLite
driver is `zombiezen.com/go/sqlite`, which is pure Go, so there is nothing to
link and cross-compiling from a Mac needs no toolchain. If the driver ever
changes to a cgo one, this recipe stops being a one-liner — treat that as part
of the cost of the change.

Ship them:

```sh
rsync -av deploy/linux/amd64/ cms:/opt/cms/bin/
```

`cms` is a `~/.ssh/config` host alias for the droplet — see `PROVISIONING.md`,
step 1. There is deliberately no `--chmod=F755`: recent macOS ships openrsync,
which rejects it, and `make release` already leaves the binaries `755` for
`rsync -a` to preserve. Then restart the service: "Deploying a new version" below has the order,
which is not simply `systemctl restart`. `deploy/linux/` is build output and is
not committed.

Two checks worth doing once, on the server, before the first restart:

```sh
/opt/cms/bin/cmsd version                 # expect: panics, CMS_ENV is not set
CMS_ENV=production /opt/cms/bin/cmsd version
```

The first panicking is the interlock working. If it prints a version instead,
the binary was built without `-tags production` and must not be deployed.

## Deploying a new version

The first deploy is `PROVISIONING.md`. Every one after it is this:

```sh
make check
make release
rsync -av deploy/linux/amd64/ cms:/opt/cms/bin/
rsync -av --exclude=linux/ deploy/ cms:/opt/cms/deploy/
```

Then, on the droplet:

```sh
sudo systemctl stop cms
CMS_ENV=production /opt/cms/bin/cmsdb migrate status --db /opt/cms/var
CMS_ENV=production /opt/cms/bin/cmsdb migrate up --db /opt/cms/var
sudo systemctl start cms
```

**The order is the point.** `cmsd` requires `PRAGMA user_version` to equal the
number of migrations the binary embeds, and refuses to start otherwise (§13.4)
— so a new binary with a pending migration will not run, which is the intended
behaviour and not a bug to work around. The service is stopped first because
`cmsdb` and `cmsd` would otherwise contend for the same SQLite write lock.

Take the backup before the migration, not after:

```sh
sqlite3 /opt/cms/var/cms.db ".backup '/opt/cms/backups/cms-$(date +%F).db'"
```

**With the service stopped, copying `cms.db` alone is enough.** A graceful
shutdown checkpoints the write-ahead log back into the database file and logs
that it did (`msg="database closed" ... wal=truncated`), so there is nothing
left in `cms.db-wal` for the copy to miss. That is not true of a *running*
server or of one that was killed: there, the log holds commits the database
file does not, and a backup has to be `sqlite3 .backup` or all three of
`cms.db`, `cms.db-wal`, and `cms.db-shm` copied as a set.

Migrations are append-only after beta. Until then a squash is a deliberate,
separately announced event — and on a database that has been deployed, it is a
restore-from-backup, not a `migrate up`.

If the unit file or a proxy config changed in the same push, install it from
`/opt/cms/deploy/` and reload that service; the copies under `/etc` are meant
to be diffable against the originals shipped there.
