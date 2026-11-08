# Installation, update and removal

## Supported environment

One Linux host using systemd, Docker Engine and Compose v2 (2.24+), Git and an
OpenSSH client. The service user must access the local Docker socket. A rootless
Docker setup needs its own socket/context and systemd overrides; it is not part
of the initial tested installation path. Native macOS and Windows daemons are
unsupported. Docker tests on a Mac use Linux containers in a VM.

For a local Linux machine, polling requires no inbound port. For webhooks or
remote panel access, use an HTTPS reverse proxy. No provider account is required
for public Git repositories. Private SSH access needs a read-only deploy key.

## Reproducible build

Use Go **1.27.3**, the pinned `go.mod` and `go.sum`, and a fixed source revision.
Get Go archives from `https://go.dev/dl/` and verify their SHA-256 checksum:

| Archive | SHA-256 |
| --- | --- |
| go1.27.3.linux-amd64.tar.gz | `65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40` |
| go1.27.3.linux-arm64.tar.gz | `a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49` |

```sh
go mod download
go mod verify
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o bin/repodeploy ./cmd/repodeploy
sha256sum bin/repodeploy
```

Use `GOARCH=arm64` for ARM hosts. Pinning the deployer build does not make an
application Docker build reproducible: pin base images and application dependencies
separately if that property is required.

## Install on the host

Install Git, OpenSSH client, Docker Engine and Compose following your distribution
and Docker's documentation. The installer does not alter their configuration or
install system packages. Verify `docker info` and `docker compose version` first.

```sh
sudo sh scripts/install.sh bin/repodeploy
```

The script installs the binary, service account, state directory and systemd unit.
It grants the account membership in the existing `docker` group. **This grants
root-equivalent host control.** Repository code must be trusted. Container security
flags are defense in depth and do not turn this service into an untrusted-code
sandbox. The unit adds kernel, device, namespace, address-family and system-call
restrictions (`systemd-analyze security repodeploy` rates it 3.4–3.6 "OK",
previously 9.0 "UNSAFE"); a full SSH deployment, polling, backup and deletion were
verified under it on Ubuntu 26.04 with systemd 259. If a site-specific override
needs more, relax one setting at a time with `systemctl edit repodeploy`.

Initialize credentials once, using a password file outside Git:

```sh
sudo -u repodeploy /usr/local/bin/repodeploy init --password-file - < /secure/path/admin-password
sudo systemctl enable --now repodeploy
sudo journalctl -u repodeploy -n 50
```

No default password exists. Initialization refuses to overwrite credentials.
`admin.json` stores an Argon2id password hash (with its parameters) and the CLI
token hash; `admin.token` contains the actual CLI token. The directory is `0700`
and credential files are `0600`. Only the service user and host administrator
should read them. Do not publish or commit a state-directory backup.

### Using the CLI without sudo

The CLI only needs the daemon URL and the token. Copy the token once to your own
account and keep it private; the CLI refuses token files readable by group or
others:

```sh
install -d -m 0700 ~/.config/repodeploy
sudo cat /var/lib/repodeploy/admin.token > ~/.config/repodeploy/token
chmod 600 ~/.config/repodeploy/token
repodeploy projects
```

Token lookup order: `--token-file`, `$REPODEPLOY_TOKEN_FILE`, the user's config
directory (`~/.config/repodeploy/token` on Linux), then `admin.token` in the state
directory. `$REPODEPLOY_URL` sets the default daemon URL (HTTPS is required for
non-loopback hosts). After `rotate-token`, copy the token again.

### Changing the password and rotating the CLI token

Use **Security** in the panel, or the CLI as the service user. Both work while
the daemon runs; it reloads `admin.json` on the next request.

```sh
sudo -u repodeploy repodeploy passwd --password-file - < /secure/path/new-password
sudo -u repodeploy repodeploy rotate-token
```

Changing the password signs out every browser session. Rotating the token
revokes the previous one immediately and writes the new value to `admin.token`
(the CLI never prints it); copy it to any remote CLI machine. Browser sessions
stay signed in.

Hashes created by earlier versions (Argon2id, 1 pass) are re-derived with the
current parameters (3 passes, 64 MiB) after the next successful sign-in.

## SSH repository access

The graphical SSH assistant under **Repository connections → SSH access** handles
key generation and host verification for github.com and bitbucket.org. The manual
file setup below remains available for other SSH servers or existing keys.

Place keys under `/var/lib/repodeploy/keys/`, owner `repodeploy`, directory `0700`,
private key `0600`. Register the public key as read-only with the Git provider.
Provision `known_hosts` from fingerprints verified through a trusted channel.
`ssh-keyscan` output alone is not proof of server identity.

The systemd unit hides home directories. Use paths under the state directory
or another path explicitly exposed by an override. No interactive passphrase
or SSH-agent authentication is used.

## HTTPS reverse proxy

Keep the service bound to loopback and terminate TLS with your existing proxy.
Enable secure session cookies and per-client login throttling using a service
override:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/repodeploy --state /var/lib/repodeploy serve --listen 127.0.0.1:8081 --public-url https://deploy.example.org --trust-proxy
```

Example Nginx location inside an HTTPS server block:

```nginx
# Project uploads (panel and `repodeploy up`).
location ~ ^/(api/)?projects/[^/]+/uploads$ {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_request_buffering off;      # stream archives straight to RepoDeploy
    proxy_read_timeout 600s;
    client_max_body_size 200m;        # at least serve --max-upload
}

location / {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    client_max_body_size 1m;
}

# GitHub push payloads can reach 25 MB.
location /hooks/github/ {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    client_max_body_size 25m;
}
```

The upload location needs a `client_max_body_size` at least as large as
`serve --max-upload` (default 200MB); otherwise nginx answers `413` before
RepoDeploy sees the archive.

Do not expose port `8081` directly to untrusted networks. A non-loopback bind
requires `--public-url https://...`, but the application itself still serves HTTP;
you must actually provide TLS termination. Browser writes require CSRF tokens;
CLI requests use the separate bearer token.

With `--public-url` the panel also sends `Strict-Transport-Security` and names its
session cookie `__Host-repodeploy_session` (provider-connection cookies use the
`__Secure-` prefix), so browsers only accept them over HTTPS and a sibling
subdomain cannot overwrite them. Webhook bodies may be up to 25 MiB (GitHub's
limit); project uploads accept up to `--max-upload` (default 200 MB), at most two
at a time (`429` with `Retry-After` otherwise); other requests are limited to
1 MiB.

**Host validation.** The panel answers only requests whose `Host` is `localhost`,
a loopback address, or the host of `--public-url`; anything else receives
`421 Misdirected Request`. This blocks DNS-rebinding attacks against the loopback
panel. The proxy must forward the original `Host` (as above). To accept extra
names, such as a VPN host name, add `--allowed-hosts name1,name2`.

**Login throttling.** Each client may make 5 sign-in attempts per minute; after
5 consecutive failures it is locked out for 1 minute, doubling up to 15 minutes.
IPv6 addresses in one /64 share a budget. Password verification is limited to
two concurrent operations and a bounded queue; traffic from other networks
cannot exhaust a global minute-long allowance. Client tracking is bounded.
Clients are identified by their TCP peer address. Behind a reverse proxy every request
comes from `127.0.0.1`, so add `--trust-proxy`: the rightmost
`X-Forwarded-For` entry (or `X-Real-IP`) is then used, but only for requests
from a loopback peer. Without it, all proxied clients share one limit.

## Upgrade from Pushvori

The product, binary and systemd service are now named RepoDeploy/`repodeploy`.
New installations use account `repodeploy` and `/var/lib/repodeploy`. An existing
installation keeps account `pushvori` and `/var/lib/pushvori`: persisted releases
and SSH configuration contain absolute paths, so renaming that directory would
break them. Both CLI and installer select existing new state first, then legacy
state; `--state` always takes precedence for the CLI.

Build and test the new binary first. Stop the old service and take a cold backup
of the entire state directory, preserving permissions, plus a copy of the old
binary. Run:

```sh
sudo systemctl stop pushvori
# Back up /var/lib/pushvori and /usr/local/bin/pushvori before proceeding.
sudo sh scripts/install.sh bin/repodeploy
sudo systemctl enable --now repodeploy
sudo -u pushvori repodeploy projects
```

The installer refuses replacement while either service is running and disables
the old service to avoid two daemons starting on reboot. It does not reinitialize
credentials, move state or replace application containers. **Use your existing
panel password.** Sign in again after the service restart. Legacy systemd overrides
(custom address, HTTPS URL or limits) must be applied to `repodeploy.service`
before starting it; use the new executable and keep the existing `--state` path.
For manual SSH/secret commands on an upgraded host, retain `sudo -u pushvori` and
the legacy state paths. Existing provider App registrations remain valid; only
newly registered Apps use the new name.

Docker project/volume names (`pv-*`), generated image namespaces (`pushvori/`) and
labels (`io.pushvori.*`) remain stable so existing releases, rollback and retention
continue to address the same resources. There is no application-data migration.
The old binary/unit are retained for a stopped-service downgrade if needed.

## Backup and update

`repodeploy backup` writes a consistent archive while the daemon keeps running:
the database is copied with SQLite `VACUUM INTO`, and it includes credentials,
provider connections, generated SSH keys, managed secrets, logs and release
manifests (Git checkouts are skipped). The file is created with mode `0600` and
is never overwritten. It contains secrets: store it encrypted and off-host.

```sh
sudo -u repodeploy repodeploy backup --output /var/lib/repodeploy-backups/state-$(date +%F).tar.gz
```

To restore, stop the service and extract into an **empty** state directory, so
no stale `state.db-wal`/`state.db-shm` files from another database remain:

```sh
sudo systemctl stop repodeploy
sudo mv /var/lib/repodeploy /var/lib/repodeploy.old
sudo install -d -o repodeploy -g repodeploy -m 0700 /var/lib/repodeploy
sudo -u repodeploy tar -xzf /path/to/state.tar.gz -C /var/lib/repodeploy
sudo systemctl start repodeploy
```

Release manifests reference absolute paths, so restore into the same state
directory path. To update:

1. Stop the service and confirm its process group is stopped. systemd manages
   the worker and subprocess lifetime together; do not run two daemons against
   the same state directory.
2. Take a backup with `repodeploy backup`, or copy the **entire** stopped state
   directory preserving permissions. Back up application volumes independently.
3. Keep the old binary, build and verify the new revision, then install the new
   binary while the service is stopped. The schema currently has version 1;
   future releases must document migrations before changing it.
4. Start the service, inspect the journal, active release and deployment history.
   Startup reconciles interrupted work before the worker starts new deployments.

Do not assume an older binary can read a migrated database. Restore matching
binary and state backup if downgrading after an incompatible migration. A state
backup does not restore application databases, Docker images or volumes; save
images separately if recovery must survive Docker storage loss.

## Uninstall

```sh
sudo systemctl disable --now repodeploy
sudo rm /etc/systemd/system/repodeploy.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/repodeploy
```

These commands preserve application containers, Docker volumes, the service
account and `/var/lib/repodeploy`. Inspect and stop each managed Compose project
explicitly if desired; never use a global prune as an uninstall step. Delete
state, account or application volumes only after a separate backup and an
explicit decision to remove those data.

## Verified OrbStack installation

Verified on 2026-11-07 in the dedicated OrbStack machine
`pushvori-systemd-test`: Ubuntu 24.04.5 LTS, ARM64, systemd as PID 1,
Docker Engine 29.1.3 and Compose 2.40.3. Limits: 2 CPUs, 4 GiB RAM,
16 GiB disk. Host folder sharing and access to other machines/host IPs are
disabled. This records the earlier verification; that machine is no longer
present. The current connection-testing machine is described below.

The installed binary was built from this workspace with `CGO_ENABLED=0`,
`GOOS=linux`, `GOARCH=arm64`, `-trimpath`, `-buildvcs=false`. SHA-256:

```text
072f87a5fa8b8bfc4de373b3fe091b4a9574155d9fbd8b8290a9b61bea139c26
```

The unmodified installer and systemd unit passed:

| Check | Result |
| --- | --- |
| `scripts/install.sh bin/pushvori-linux-arm64` | Binary, account, permissions and unit installed |
| `systemd-analyze verify /etc/systemd/system/pushvori.service` | Passed |
| Docker access as `pushvori` | Passed |
| `systemctl is-active/is-enabled pushvori` | `active` / `enabled` |
| Login and authenticated CLI | Passed; unauthenticated API returned 401 |
| Browser write without CSRF token | Rejected with 403 |
| Private Git using a restricted SSH key | Passed against the local read-only fixture |
| Example manual deployment | Healthy; exact commit `2fc8ec5a5339c5592da11cf872c627fde3c06798` |
| `systemctl restart pushvori` | Same active release, image, history, credentials and volume data |
| Automatic deployment after a fixture commit | Healthy; polling activated `e11344559bbba564a0b02d1864d911b499638d65` |
| `orb restart pushvori-systemd-test` | Machine boot ID and PID 1 start time changed; both services started automatically |
| Persistence after machine restart | Project, active release, image, three history entries, SSH/admin credentials and volume marker unchanged |

The example returns `second` on port 18080 and `healthy` from `/health`.
Its active release is `21bf66019f0948fbf50828801b626083`; project ID is
`1394b2c864ce4e5192fb1a93262d5f95`. The named volume retains the marker
`pushvori-systemd-persistence`. History includes the initial environment-related
build failure and both successful deployments; the failed run was not deleted.

### Docker storage adjustment for this machine

The initial build failed with `overlay ... operation not permitted`. This
OrbStack machine cannot use that nested OverlayFS configuration. Only this new
machine's `/etc/docker/daemon.json` was configured as follows, while its Docker
and Pushvori services were stopped:

```json
{
  "features": { "containerd-snapshotter": false },
  "storage-driver": "vfs"
}
```

VFS trades performance and disk use for compatibility; see Docker's
[VFS documentation](https://docs.docker.com/engine/storage/drivers/vfs-driver/).
This is a VM-specific test configuration, not a change to Pushvori's installer
or to the Mac's Docker engine. Do not switch an existing production host's
storage backend as part of these instructions: its existing images/containers
can become hidden until the original backend is restored.

### Inspect the retained test environment

```sh
orb -m pushvori-systemd-test -u root systemctl status pushvori --no-pager
orb -m pushvori-systemd-test -u root runuser -u pushvori -- /usr/local/bin/pushvori projects
orb -m pushvori-systemd-test -u root curl -fsS http://127.0.0.1:18080/
orb -m pushvori-systemd-test -u root python3 /root/pushvori-vm-verify.py machine-restart
```

The last command checks the retained baseline, not just HTTP availability.
Detailed stage snapshots are `/root/pushvori-verification-*.json` inside the
machine. The generated administrator password is in
`/root/pushvori-admin-password`, mode `0600`; it was never printed or stored in
the source repository. The Git fixture lives at `/srv/pushvori-example` and its
SSH server listens only on loopback port 2222. Git commits were created only in
that disposable fixture, without source-repository commits or any pushes.

The fresh OrbStack image already had a failed `sys-kernel-debug.mount` unit
before provisioning; Docker and Pushvori are active and have no failed restarts.
Verification waits for Docker's initial healthcheck after boot: HTTP availability
can precede Docker's transition from `starting` to `healthy`.

## Browser and SSH connections

Open **Connect GitHub, Bitbucket or SSH** in the panel. Use a stable browser address
throughout registration and later reconnections. HTTP loopback addresses are
supported for local setups; remote setups require the configured public HTTPS URL.
SSH tunnels work because the provider redirects your browser to the panel, rather
than making a server-to-server callback. No public inbound port is needed with polling.

### GitHub browser authorization

On the first connection, the panel submits a GitHub App manifest and GitHub asks
you to confirm its name. To use organization repositories, specify that organization
before starting registration. The App is private to its owning account. The panel
receives its credentials automatically; you never copy a private key or token.
Install it on the repositories you select, then return to choose repository/branch.
Read permissions: contents, metadata, checks and commit statuses. Registration uses
GitHub's [manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).
One App/installation is configured per self-hosted instance. Choose its personal
or organization owner during initial setup. Enterprise Server is outside this flow.

### Bitbucket Cloud browser authorization

Bitbucket requires you to create an OAuth consumer in your workspace once; it has
no equivalent manifest flow. The panel displays its exact callback URL and fields
for consumer key/secret. Select **Account → Read**, **Repositories → Read**, and
private consumer, with no additional scopes. Save, then click **Connect Bitbucket**
to authorize in your browser and choose workspace/repository/branch. See
[Bitbucket OAuth](https://support.atlassian.com/bitbucket-cloud/docs/use-oauth-on-bitbucket-cloud/).
An organization/workspace policy may require its administrator's approval.
This supports bitbucket.org, not Bitbucket Data Center. Bitbucket deployments use
polling/manual triggers; GitHub remains the supported webhook provider.

### Graphical SSH authorization

Enter the repository URL, generate an access key, copy its public key, and open
the linked repository settings to register it with read access. Return and click
**Check connection and list branches**. Host keys are fetched from the provider's
official HTTPS key source, not trusted from unauthenticated SSH keyscan output.
The browser never receives the private key. Existing SSH paths remain an advanced
configuration option; manual shell commands are not required by the assistant.

### CI/CD waiting

Each project chooses immediate deployment (default) or waiting for CI/CD. The gate
checks the exact pinned commit: latest GitHub check runs plus commit statuses, or
Bitbucket commit build statuses. All reported results must pass. GitHub neutral/
skipped checks count as completed; failed/cancelled/timed-out results fail the
deployment. No results means waiting, not success. The timeout defaults to 1800s,
configurable from 30s to 86400s, with rechecks about every ten seconds.

Waiting jobs remain durable as `waiting_ci`; other projects continue, while later
jobs of that project preserve FIFO order. Restart resumes waiting; rollback cancels
outstanding waits and restores the retained release without rerunning CI. Changing
project settings affects new jobs, because existing jobs keep their configuration
snapshot. There is no dependency on GitHub Actions: any CI reporting provider
statuses can be used. Checks not yet published cannot be anticipated, and health
checks still run after activation. Browser provider authorization is required to
read private CI even if Git transport uses SSH. The panel validates access before
saving that policy, rather than silently ignoring it.

Provider credentials live in protected `connections.json` and generated SSH keys in
`ssh/` under the state directory; `repodeploy backup` includes both. The
connections page lists generated keys with the projects that use them. Unused
keys can be deleted there, and keys that no project or pending deployment uses
are deleted automatically 24 hours after creation, so abandoned private keys do
not accumulate. Remove the matching public key from the repository too. Installation tokens
are repository-scoped and cached in memory; Bitbucket refresh-token rotation is
persisted atomically before reuse. Tokens are never stored in repository URLs,
checkout configuration, job snapshots, HTML or build arguments. A service restart
during browser authorization requires starting that browser flow again; existing
connections survive. Revoked/expired access yields reconnect errors and preserves
the active application. Disconnect is local; revoke the provider App separately
when access should also be removed at GitHub/Bitbucket.

### Current OrbStack test instance

The current instance is `pushvori-connect-test`, an isolated Ubuntu 24.04.5 ARM64
machine with systemd, Docker 29.1.3 and Compose 2.40.3. The same VFS configuration
documented above is used. The example deployment and generated SSH keys survived
a complete machine restart. It has been upgraded to RepoDeploy: `repodeploy.service`
is enabled and active; the old service is disabled. Account/state remain
`pushvori` and `/var/lib/pushvori`. The existing password, both history entries,
active image/release, volume marker and all 17 credential-file hashes matched
before and after the upgrade and another complete VM restart. Root-only evidence:
`/root/repodeploy-rename-verification-*.json`. Provider API flows
are covered by tests, but real account authorization is left to the administrator.

Run these commands **on the Mac**, not inside the Linux machine. Copy the panel
password without printing it:

```sh
orb -m pushvori-connect-test -u root cat /root/pushvori-admin-password | pbcopy
```

The verified local panel is <http://localhost:18083/connections> and the example
is <http://localhost:18084/> while the SSH tunnel is running. If that tunnel has
closed, leave this command running in a Mac terminal:

```sh
ssh -N -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:18083:127.0.0.1:8081 \
  -L 127.0.0.1:18084:127.0.0.1:18080 \
  root@pushvori-connect-test@orb
```

Keep the same localhost address throughout provider registration so callback
URLs match. The panel stays bound to loopback inside the machine and requires
authentication through the tunnel.
