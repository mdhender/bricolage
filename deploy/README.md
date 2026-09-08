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

```sh
caddy run --config deploy/Caddyfile.dev
go build -tags dev -o bin/cmsd ./cmd/cmsd
bin/cmsd serve --db ./dev.db --addr 127.0.0.1:18443 --env development --timeout 60m
```

Open <https://htmx-app.localhost:8443/>.

`-tags dev` plus `--env development` enables the `/__development/*` routes,
which let an
automated agent log in without a password and stop the server over HTTP. They
are absent from any build made without the tag. `--timeout` is ungated and
useful anywhere; always pass one in development so an abandoned server does not
sit on the SQLite lock.

`earl` points at the same public URL, not at the Go listener:

```sh
earl login --server https://htmx-app.localhost:8443
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
| `environment` | leave unset, or `production`. Never `development`. |

Production binaries are built **without** `-tags dev`. CI asserts that a default
build returns 404 for every `/__development/*` route; that assertion gates
release.
