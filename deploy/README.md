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
go run ./cmd/cmsd serve --db ./dev.db --addr 127.0.0.1:18443 --env development --timeout 60m
```

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

Set in `cmsd` configuration:

| Key | Value |
|---|---|
| `server.addr` | a loopback address and port |
| `server.public_origin` | the browser-facing origin, scheme included |
| `server.trusted_proxies` | CIDRs the proxy connects from |
| `server.timeout` | optional graceful shutdown after a duration; `0` = never |
| `environment` | `production`, exported as `CMS_ENV` — see below |

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
rsync -av --chmod=F755 deploy/linux/amd64/ deploy@SERVER:/opt/cms/bin/
```

Then restart the service on the server. `deploy/linux/` is build output and is
not committed.

Two checks worth doing once, on the server, before the first restart:

```sh
/opt/cms/bin/cmsd version                 # expect: panics, CMS_ENV is not set
CMS_ENV=production /opt/cms/bin/cmsd version
```

The first panicking is the interlock working. If it prints a version instead,
the binary was built without `-tags production` and must not be deployed.
