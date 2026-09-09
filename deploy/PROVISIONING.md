# Provisioning a production droplet

This is the first-time setup for `cms.mdhenderson.com` on a DigitalOcean
droplet: the machine, the SSH keys, the directory layout, the reverse proxy,
and the first deploy. Do it once. Afterwards, `deploy/README.md`, "Deploying a
new version", is the whole of the routine.

The shape being built is the one `docs/DESIGN.md` §11 requires: a reverse proxy
terminating TLS on the public interface, and `cmsd` speaking plain HTTP on
loopback behind it, on the same droplet. `cmsd` never terminates TLS, never
creates a directory, and never migrates a database. Every step below that looks
like busywork — the `mkdir` list, the ordering of migrate and restart — is
there because the application deliberately refuses to do it for you.

## 1. The droplet

Ubuntu 26.04 LTS. The smallest shared-CPU droplet is enough to begin with:
the database is SQLite, the binaries are static, and there is no runtime to
host. What will grow first is disk, because the output tree and the preview
tree are both files on it; give it room or attach a volume later.

Create it with your SSH key attached, from the "SSH keys" section of the
create form. Do not create it with a root password — a droplet with password
authentication is being brute-forced within the hour, and adding the key
afterwards leaves that window open.

If you do not already have a key for this:

```sh
ssh-keygen -t ed25519 -C "mdhenderson.com deploy" -f ~/.ssh/id_ed25519_mdhenderson
pbcopy < ~/.ssh/id_ed25519_mdhenderson.pub    # paste into DigitalOcean
```

Give it a passphrase and let the macOS keychain hold it, so the passphrase is
not a reason to reach for an unprotected key later:

```sh
ssh-add --apple-use-keychain ~/.ssh/id_ed25519_mdhenderson
```

Then name the host once, in `~/.ssh/config`, so that every command in this
document and in `README.md` can say `cms` and mean the same thing:

```
Host cms
    HostName 203.0.113.10          # the droplet's IPv4 address
    User deploy
    IdentityFile ~/.ssh/id_ed25519_mdhenderson
    IdentitiesOnly yes
```

`User deploy` is deliberate even though that account does not exist yet; the
next step is where it is made, and until then connect explicitly as
`ssh root@203.0.113.10`.

## 2. DNS

Point `cms.mdhenderson.com` at the droplet before going any further. Both
proxies below obtain a certificate by proving control of the name over port 80,
and neither can do that while the record is missing or stale.

An `A` record to the IPv4 address; an `AAAA` record to the IPv6 address if you
enabled it (both proxy configs listen on `[::]`). Confirm from the Mac before
continuing, and ask a resolver that is not your cache:

```sh
dig +short cms.mdhenderson.com @1.1.1.1
```

## 3. The deploy user, and closing the front door

As `root` on the droplet:

```sh
apt update && apt upgrade -y
apt install -y ufw

adduser --disabled-password --gecos "" deploy
install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
install -m 600 -o deploy -g deploy /root/.ssh/authorized_keys /home/deploy/.ssh/authorized_keys
adduser deploy sudo
```

`deploy` owns `/opt/cms` and runs the service. On a droplet with one operator
that is the right number of accounts. If more than one person ever deploys,
split it: a `cms` system account that runs the unit and cannot write
`/opt/cms/bin`, and a `deploy` account that can. That is a one-line change to
`User=` in the unit plus the ownership of the binaries, and it is worth doing
at the point where "who pushed that binary" stops having an obvious answer.

Now confirm — **from a second terminal, before closing the first** — that
`ssh cms` works and `sudo -v` succeeds. Locking yourself out of a droplet is
recoverable only through the console.

Then close password and root login. In `/etc/ssh/sshd_config`:

```
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
```

Ubuntu 26.04 ships drop-ins under `/etc/ssh/sshd_config.d/` that can override
that file, and the cloud image usually leaves one there. Check for it rather
than assuming, then reload:

```sh
grep -rn "PasswordAuthentication\|PermitRootLogin" /etc/ssh/sshd_config.d/ /etc/ssh/sshd_config
sshd -t && systemctl reload ssh
```

Firewall. The only ports that need to be open are SSH and the two the proxy
listens on. `cmsd` is on loopback and must never be reachable from outside —
it is a TLS-less server that trusts `X-Forwarded-*` from anything in its
trusted-proxy list, and its listen address is the thing keeping that honest:

```sh
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable
ufw status verbose
```

Nothing opens 18443. If you ever need to reach `cmsd` directly to debug the
proxy, tunnel it: `ssh -L 18443:127.0.0.1:18443 cms`.

## 4. The directory layout

Nothing in this system creates a directory it was told to use (invariant 19).
Not `cmsd`, not `cmsdb`, not a test helper. The single exception is the
computed *interior* of the output tree — `/features/film/2026/03/01/` — which
`internal/publish/tree.go` creates because it is arithmetic over a category
path and a URI format rather than a path anybody typed. The root of that tree
is still never created.

So this list is not boilerplate. Every one of these is a directory the server
will refuse to start without, by name, in one log line:

```sh
sudo install -d -o deploy -g deploy /opt/cms /opt/cms/bin /opt/cms/var \
    /opt/cms/templates /opt/cms/preview /opt/cms/output /opt/cms/backups
```

| Path | Flag | What lives there |
|---|---|---|
| `/opt/cms/bin` | — | `cmsd`, `cmsdb`, `earl` |
| `/opt/cms/var` | `--db` | `cms.db`, plus SQLite's `-wal` and `-shm` beside it |
| `/opt/cms/templates` | `--templates` | the content template tree, `<site domain>/<category path>/<element type>.gohtml` |
| `/opt/cms/preview` | `--preview` | flat, content-addressed `<sha256>.<ext>` |
| `/opt/cms/output` | `--output` | published files; the interior is created, this root is not |
| `/opt/cms/backups` | — | see "Backups" below |

`/opt/cms/var` needs to be writable as a *directory*, not just the file:
SQLite creates the write-ahead log and shared-memory files next to the database.
The unit's `ReadWritePaths` already says so, and `templates` is deliberately
absent from it — `internal/render` returns bytes and never writes.

## 5. The reverse proxy

Pick one. `deploy/Caddyfile.prod` and `deploy/nginx.conf` are alternatives, not
layers, and each carries its own installation notes at the top.

Caddy is the shorter path: it obtains and renews the certificate itself, with
no certbot and no renewal hook to forget.

```sh
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy

sudo install -d -o caddy -g caddy /var/log/caddy
sudo install -m 644 /opt/cms/deploy/Caddyfile.prod /etc/caddy/Caddyfile
sudo systemctl restart caddy
```

The `install -d` is not optional. Caddy creates the log *file* but not the
directory above it, and the packaged unit runs as `User=caddy` under
`ProtectSystem=full`. Nor should you run `caddy validate` on that config as
root: validation loads the config, which opens the log writer, which leaves
`cms.access.log` owned by `root:root` mode `0600` — and the service then fails
to start with `permission denied` on a file that exists, in a directory the
`caddy` user can write. `sudo chown -R caddy:caddy /var/log/caddy` undoes it.

The Ubuntu archive has Caddy 2.6.2; the Cloudsmith repository above installs
2.11.4. Take the newer one — this is the component holding your certificate
lifecycle.

For nginx instead, follow the header comment in `deploy/nginx.conf`: install
`nginx` and `certbot`, put the file in `sites-available`, issue the certificate
in webroot mode, and add the deploy hook that reloads nginx on renewal.

Whichever you choose, the proxy has to satisfy four things the application
depends on, and both files here already do:

1. **TLS 1.3 or better**, stated explicitly — Caddy's own floor is 1.2.
2. **The forwarded headers set**: `X-Forwarded-For`, `-Proto`, `-Host`.
   `cmsd` honours them only from its trusted-proxy CIDRs, which default to
   loopback — correct here, because the proxy is on this droplet.
3. **`Origin` and `Sec-Fetch-*` passed through unmodified.**
   `net/http.CrossOriginProtection` reads exactly those to decide whether a
   cookie-authenticated write is CSRF. A proxy that rewrites `Origin` breaks
   every form in the UI, and it breaks them as a 403 that looks like a
   permissions bug.
4. **HSTS and the HTTP→HTTPS redirect belong to the proxy.** `cmsd` emits no
   HSTS header of its own and must not be made to.

The proxy will fail to start until `cmsd` is answering — that is fine and
expected at this point in the order; finish the deploy below and reload it.

## 6. The first deploy

Build on the Mac. Cross-compiling needs no toolchain because the SQLite driver
is `zombiezen.com/go/sqlite`, which is pure Go:

```sh
make check
make release          # -tags production, into deploy/linux/amd64/
rsync -av deploy/linux/amd64/ cms:/opt/cms/bin/
```

No `--chmod` here. Recent macOS ships openrsync, which reports itself as
"rsync version 2.6.9 compatible" and rejects `--chmod=F755` as an invalid
argument. It is not needed: `make release` leaves the binaries `755` and
`rsync -a` preserves that.

Ship the deploy directory too, so the unit and proxy config on the droplet are
diffable against the originals:

```sh
rsync -av --exclude=linux/ deploy/ cms:/opt/cms/deploy/
```

**Verify the interlock before anything else.** On the droplet:

```sh
/opt/cms/bin/cmsd version                        # expect: panic, CMS_ENV is not set
CMS_ENV=production /opt/cms/bin/cmsd version     # expect: the version
```

The first one panicking is the check working. If it prints a version instead,
the binary was built without `-tags production` — it must not be deployed, and
the fix is on the Mac, not here.

Create and seed the database. `cmsd` has no `--migrate` flag and no `--create`
flag, because there is no way to say yes; `cmsdb` is the only thing that
touches the schema:

```sh
export CMS_ENV=production        # for this shell only, and only for these commands
/opt/cms/bin/cmsdb init --db /opt/cms/var
/opt/cms/bin/cmsdb seed --db /opt/cms/var
/opt/cms/bin/cmsdb bootstrap admin --db /opt/cms/var \
    --email you@example.com --name "Your Name"
```

**`seed` comes before `bootstrap admin`, and the order is not cosmetic.** The
`admin` role is one of the things `seed` writes, and `bootstrap admin` assigns
it to the user it creates. Run them the other way round and the note it prints
is easy to miss:

```
note: there is no "admin" role yet, so no role was assigned; run "cmsdb seed" and try again
```

"Try again" is not advice that works. `bootstrap admin` is idempotent by
refusing — a second run with the same email exits non-zero with *a user with
that email already exists; nothing was changed* — so the role is never
assigned, and you are left with an administrator who cannot administer
anything. On a database this fresh the fix is to delete `cms.db` and its `-wal`
and `-shm` and start the three commands over; on one with content in it, it is
a role assignment somebody has to write by hand.

`bootstrap admin` prints a generated password **once**, to stdout. Capture it
now; there is no second chance and no flag that would let you pass one in
(arguments are visible in `ps` and land in shell history). `seed` writes the
roles, the site, the root category, an output channel, and the element types —
not the workflow, which migration 0005 seeds, because `documents.workflow_id`
is `NOT NULL` and no document row may exist before a workflow does.

The site `seed` writes is `htmx-app.localhost`, which is the development public
origin and wrong on any real server. That name is also the first path segment
of the template tree — `<templates>/<site domain>/<category path>/<element
type>.gohtml` — so until it can be changed, a deployment must either lay its
templates out under a directory named `htmx-app.localhost/` or rewrite the row
with hand-written SQL. Neither is good and both are temporary: see issue #3.
Expect to settle it before anything renders or publishes.

Do not leave `CMS_ENV` exported in a shell profile. The unit sets it, and that
is the only place it should live.

## 7. Start it

```sh
sudo install -m 644 /opt/cms/deploy/cms.service /etc/systemd/system/cms.service
sudo systemctl daemon-reload
sudo systemctl enable --now cms
systemctl status cms
sudo systemctl reload caddy        # or: sudo systemctl reload nginx
```

## 8. Verify

Four checks, in the order that isolates a failure to one layer.

```sh
# 1. cmsd itself, on the droplet, past the proxy entirely.
curl -si http://127.0.0.1:18443/healthz | head -1

# 2. Through the proxy, from the Mac. TLS, redirect, HSTS.
/usr/bin/curl -sI https://cms.mdhenderson.com/healthz
/usr/bin/curl -sI http://cms.mdhenderson.com/ | grep -i location

# 3. The development routes are absent. This is the one that matters most.
/usr/bin/curl -so /dev/null -w '%{http_code}\n' \
    https://cms.mdhenderson.com/__development/shut-it-down     # expect 404

# 4. End to end, as a user.
go run ./cmd/earl login --server https://cms.mdhenderson.com
go run ./cmd/earl whoami --server https://cms.mdhenderson.com
```

Check 3 answering anything but 404 means the resolved environment is
`development`, and every account on the system is open to anyone who guesses an
email address. `cmsd routes` on the droplet prints the table the running
configuration produces and names the resolved environment, which is the way to
ask the question directly:

```sh
CMS_ENV=production /opt/cms/bin/cmsd routes | head -3
```

Use `/usr/bin/curl` when you are looking at TLS. Homebrew's `curl` and
`openssl` carry their own CA bundles and will disagree with the system trust
store, which turns a certificate question into an afternoon.

## 9. Backups

Two things are state, and they are not the same kind of thing.

`/opt/cms/var/cms.db` is the system of record. Back it up with SQLite's own
backup, not `cp` — a copy taken while the write-ahead log is live is a torn
database, and it will restore without complaining:

```sh
sqlite3 /opt/cms/var/cms.db ".backup '/opt/cms/backups/cms-$(date +%F).db'"
```

That is safe against a running server. Get it off the droplet: a backup on the
same disk as the thing it is backing up is a copy, not a backup.

`/opt/cms/output` is the published tree, and it is reproducible — every file in
it is claimed by a row in `published_resources`. Back it up if regenerating it
is slower than restoring it, which it will be once the site is large. Either
way, `cmsdb check --output /opt/cms/output` is what reconciles the two, and it
reports the two opposite orphans — rows whose file is gone, files no row claims
— separately, because they are different failures.

`/opt/cms/preview` is a cache. Content-addressed, regenerated on demand, and
not worth a backup.

`cmsdb vacuum` and `cmsdb check` both want the database to themselves. Run them
with the service stopped, or accept that they will contend with it for the
write lock.
