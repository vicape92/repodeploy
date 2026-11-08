# RepoDeploy

Self-hosted Git-to-Docker deployments for one Linux host and one administrator.

Push to a branch and RepoDeploy checks out that exact commit, builds it with
Docker or Docker Compose while the current version keeps running, health-checks
the new version, and switches over or restores the previous one. A single
static binary provides the daemon, a web panel and a CLI. No GitHub Actions,
SaaS account, hosted database or external registry is required; repository
access and image downloads still need network connectivity.

> [!WARNING]
> **Status: early MVP.** Deploy only repositories you trust, on a host you
> administer. Access to Docker is root-equivalent on that host, and container
> hardening is defense in depth, not a sandbox for untrusted code.

## Contents

- [Features](#features)
- [How a deployment works](#how-a-deployment-works)
- [Requirements](#requirements)
- [Installation](#installation)
- [First run](#first-run)
- [Your first project](#your-first-project)
- [CLI reference](#cli-reference)
- [Project configuration](#project-configuration)
- [GitHub webhooks](#github-webhooks)
- [Security model](#security-model)
- [Backup, update and removal](#backup-update-and-removal)
- [Upgrading from earlier versions](#upgrading-from-earlier-versions)
- [Development](#development)
- [Scope and limitations](#scope-and-limitations)
- [Documentation](#documentation)

## Features

**Sources and triggers**
- Public HTTPS and private SSH repositories, one branch per project.
- Signed GitHub push webhooks, outbound polling, and manual deployments.
- Browser authorization for GitHub Apps and Bitbucket Cloud (OAuth), plus a
  graphical SSH key assistant: no GitHub CLI or terminal key handling needed.
- Optional waiting for the commit's CI/CD statuses before building or activating.

**Builds and releases**
- Exact-commit, isolated checkouts; a Dockerfile or a validated Compose subset.
- Build while the current version runs; HTTP or Compose health checks with
  automatic recovery of the previous version.
- Retained release history and logs, confirmed manual rollback, and
  reconciliation after daemon restarts.
- Managed Compose secrets and configs from the panel or CLI.
- Optional re-pull of base and service images on every deployment.

**Operations**
- One durable deployment worker shared by the web panel and the CLI.
- Live dashboard, streamed deployment logs with secret masking.
- Project deletion, with optional removal of its volumes.
- Consistent online backups of the whole state directory.

**Security**
- Password-protected panel and token-authenticated CLI API.
- Per-client login throttling, `Host` validation against DNS rebinding, a strict
  Content-Security-Policy, HSTS and `__Host-` cookies behind HTTPS.
- Password change and CLI token rotation from the panel or CLI.
- Containers run with `no-new-privileges` and rotated logs; Dockerfile-mode
  ports are published on `127.0.0.1` unless configured otherwise.
- A sandboxed systemd unit (`systemd-analyze security`: 3.4–3.6 "OK").

## How a deployment works

1. A webhook, a poll, or a manual request queues a deployment for the
   configured branch head.
2. Optionally, RepoDeploy waits for the commit's CI/CD checks to pass.
3. The exact commit is checked out into an isolated release directory.
4. Images are built while the current version keeps serving traffic.
5. The new version is started and health-checked.
6. On success it becomes active and old releases are pruned. On failure the
   previous version is restored, and the log explains why.

Activation can briefly interrupt traffic. Restoring containers **does not undo
database migrations or writes**. If recovery itself fails, the project is
blocked until you intervene. A manual rollback pauses automatic deployments
until you resume them.

## Requirements

**Host running the daemon**
- Linux with systemd (amd64 or arm64).
- Docker Engine and Docker Compose v2.24 or newer.
- Git and the OpenSSH client.

**Building**
- Go 1.26 or newer. Release builds use Go 1.27.3; see
  [reproducible build](docs/installation.md#reproducible-build).

macOS and Windows daemons are not supported. The CLI also builds on macOS and
can manage a remote daemon.

## Installation

### Option 1: Install the daemon from source (recommended for servers)

On the Linux host:

```sh
git clone https://github.com/vicape92/repodeploy.git
cd repodeploy
go test ./...
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o bin/repodeploy ./cmd/repodeploy
sudo sh scripts/install.sh bin/repodeploy
```

The installer:
- checks the prerequisites;
- creates the `repodeploy` system user and adds it to the `docker` group;
- creates `/var/lib/repodeploy` (mode `0700`);
- installs `/usr/local/bin/repodeploy` and the systemd unit.

It does not install system packages, change Docker's configuration, or start
the service.

To build on another machine, cross-compile with `GOOS=linux GOARCH=amd64` (or
`arm64`), then copy the binary to the host. The installer and the unit file
still come from a clone of this repository.

### Option 2: `go install`

```sh
go install github.com/vicape92/repodeploy/cmd/repodeploy@latest
```

This installs only the `repodeploy` binary, in `$(go env GOPATH)/bin`. Use it:

- **As a CLI on your workstation** (Linux or macOS), to manage a remote daemon.
  See [Using the CLI remotely](#using-the-cli-remotely).
- **As the daemon binary on a server.** Pass it to the installer from a clone of
  this repository: `sudo sh scripts/install.sh "$(go env GOPATH)/bin/repodeploy"`.

Pin a version with `@<commit>` or a tag instead of `@latest`.

## First run

1. Create a password file outside any repository, with mode `0600` and at least
   12 characters. Initialize the credentials as the service user, then start
   the daemon:

   ```sh
   sudo -u repodeploy /usr/local/bin/repodeploy init --password-file - < /secure/path/admin-password
   sudo systemctl enable --now repodeploy
   sudo journalctl -u repodeploy -n 50
   ```

   There is no default password, and `init` refuses to overwrite existing
   credentials.

2. Open the panel. It listens on `127.0.0.1:8081`, so from your machine use an
   SSH tunnel:

   ```sh
   ssh -L 8081:127.0.0.1:8081 your-server
   # then browse to http://127.0.0.1:8081
   ```

   To expose the panel or receive webhooks, put it behind an HTTPS reverse proxy
   and start it with `--public-url`. See
   [HTTPS reverse proxy](docs/installation.md#https-reverse-proxy).

### Using the CLI remotely

The CLI only needs the daemon URL and the CLI token. The token is stored in
`/var/lib/repodeploy/admin.token`.

**On the server, without `sudo`**, copy the token into your account:

```sh
install -d -m 0700 ~/.config/repodeploy
sudo cat /var/lib/repodeploy/admin.token > ~/.config/repodeploy/token
chmod 600 ~/.config/repodeploy/token
repodeploy projects
```

**From a workstation**:

1. Copy the token into a private file, mode `0600`.
2. Open the SSH tunnel shown above.
3. Run the CLI against the tunnel:

   ```sh
   export REPODEPLOY_TOKEN_FILE=~/.repodeploy-token
   repodeploy projects            # uses http://127.0.0.1:8081 by default
   ```

Remote non-loopback URLs (`--url` or `REPODEPLOY_URL`) must use HTTPS.

The CLI looks for the token in this order:
1. `--token-file`;
2. `$REPODEPLOY_TOKEN_FILE`;
3. your configuration directory: `~/.config/repodeploy/token` on Linux,
   `~/Library/Application Support/repodeploy/token` on macOS;
4. `admin.token` in the state directory.

It refuses token files readable by group or others.

## Your first project

**From the panel:**
1. Choose **Connect GitHub, Bitbucket or SSH**.
2. Authorize the provider, then pick a repository and a branch.
3. Choose a Dockerfile or a Compose file, and set the health check.
4. Select **Deploy immediately** or **Wait for CI/CD**.

See [browser and SSH connections](docs/installation.md#browser-and-ssh-connections).

**From the CLI:** describe the project in JSON (start from
[`examples/project.json`](examples/project.json)), then:

```sh
repodeploy add --file /absolute/path/project.json
repodeploy deploy PROJECT_ID
repodeploy logs JOB_ID --follow
```

**Example application:** [`examples/app`](examples/app) is a complete BusyBox
HTTP application with a Dockerfile, a Compose healthcheck, a persistent named
volume and switches to simulate failures. Copy its files to the root of a Git
repository, then point a project at it:

- Compose file `compose.yaml`;
- health service `app`;
- leave the Dockerfile and the HTTP health URL empty.

It publishes `127.0.0.1:18080`.

## CLI reference

```
repodeploy [--state DIR] [--url URL] [--token-file FILE] COMMAND
```

Global flags go before the command. `--state` defaults to `/var/lib/repodeploy`.

| Command | Description |
| --- | --- |
| `init --password-file FILE\|-` | Initialize administrator credentials |
| `serve [--listen ADDR] [--public-url URL] [--trust-proxy] [--allowed-hosts H,...]` | Run the daemon (the systemd unit does this) |
| `passwd --password-file FILE\|-` | Change the password; signs out all browsers |
| `rotate-token` | Issue a new CLI token and revoke the old one |
| `projects` | List projects |
| `project ID` | Show a project's settings |
| `add --file FILE` | Create a project from JSON |
| `update ID --file FILE` | Replace a project's settings from JSON |
| `deploy ID` | Queue a deployment of the configured branch |
| `history ID` | Show deployment history |
| `logs JOB_ID [--follow]` | Read or stream a deployment log |
| `rollback ID RELEASE_ID` | Restore a retained release; pauses automation |
| `resume ID` | Resume automation after a rollback |
| `secret ID NAME --file FILE\|-` | Store a managed Compose secret or config |
| `webhook-secret ID` | Rotate and display the GitHub webhook secret |
| `delete ID --yes [--volumes]` | Delete a project; `--volumes` also deletes its data |
| `backup --output FILE\|-` | Write a `tar.gz` backup of the state directory |

`serve` flags:

| Flag | Purpose |
| --- | --- |
| `--listen` | HTTP listen address (default `127.0.0.1:8081`) |
| `--public-url` | External HTTPS URL behind a reverse proxy; enables HSTS and secure cookies |
| `--trust-proxy` | Identify clients by `X-Forwarded-For` from a loopback proxy |
| `--allowed-hosts` | Extra `Host` names the panel accepts |

## Project configuration

Projects are created in the panel or with `repodeploy add --file`. Every
project needs `name`, `repo` and `branch`. It also uses exactly one build type:

- **Dockerfile:** set `dockerfile`, `port`, `container_port` and `health_url`.
- **Compose:** set `compose_file`, plus `health_url` or `health_service`.

| Field | Values | Default |
| --- | --- | --- |
| `name` | Display name, up to 100 characters | required |
| `repo` | HTTPS or SSH Git URL, without embedded credentials | required |
| `branch` | Branch to deploy | required |
| `mode` | `manual`, `poll`, `webhook` or `both` (webhook + polling) | required |
| `dockerfile` | Path relative to the repository root | — |
| `port` / `container_port` | Host and container ports (Dockerfile mode) | — |
| `bind_address` | Host address for the published port: a loopback address, `0.0.0.0` or `::` | `127.0.0.1` |
| `compose_file` | Path relative to the repository root | — |
| `health_url` | HTTP(S) URL on a loopback address | — |
| `health_service` | Compose service whose healthcheck must pass | — |
| `health_timeout` | Seconds, 1–300 | `60` |
| `poll_seconds` | Polling interval, 10–86400 | `60` |
| `retain` | Releases kept for rollback, 1–50 | `5` |
| `wait_ci` | Wait for CI/CD statuses (requires browser authorization) | `false` |
| `ci_timeout` | Seconds, 30–86400 | `1800` |
| `ssh_key` / `known_hosts` | Absolute paths for manual SSH access | — |
| `pull_images` | Pull newer base and `image:` images on every deployment | `false` |
| `allow_privilege_escalation` | Skip the injected `no-new-privileges` | `false` |

For example, a Compose project polled every minute:

```json
{
  "name": "example",
  "repo": "https://github.com/YOUR_ACCOUNT/YOUR_REPO.git",
  "branch": "main",
  "mode": "poll",
  "compose_file": "compose.yaml",
  "health_service": "app",
  "health_timeout": 30,
  "poll_seconds": 60,
  "retain": 5
}
```

The supported Compose subset, `$$` escaping, published ports, secret masking
and failure behavior are described in [docs/operations.md](docs/operations.md).

**Private repositories:** browser authorization manages credentials
automatically. The SSH assistant generates a key on the host, shows only the
public key, and checks access before listing branches.

For other SSH servers, provide two files readable by the service user:
- an unencrypted, read-only deploy key;
- a **verified** `known_hosts` file.

The SSH agent, password prompts and trust-on-first-use are disabled. Submodules
and Git LFS are not supported.

## GitHub webhooks

1. Set the project mode to `webhook` or `both`. Webhooks support github.com
   repositories only.
2. Generate a secret in the panel, or with `repodeploy webhook-secret PROJECT_ID`.
3. In the GitHub repository settings, add a webhook with these settings:
   - URL: `https://your-host/hooks/github/PROJECT_ID`
   - content type: JSON
   - events: push only
   - secret: the one generated in step 2
   - SSL verification: enabled

The daemon must be reachable over HTTPS. Allow request bodies up to 25 MB on
`/hooks/github/` in your proxy. The handler stores the work before replying
`202`, and the build runs asynchronously.

## Security model

- **Trust boundary.** RepoDeploy runs trusted code. The service user belongs to
  the `docker` group, which is root-equivalent.
- **Credentials.** The state directory is `0700`, and its credential files are
  `0600`.
  - `admin.json` holds an Argon2id password hash and the CLI token hash.
  - `admin.token` holds the CLI token itself.

  Never commit or publish a state directory or a backup.
- **Network.** The panel listens on loopback by default. Expose it only through
  an HTTPS reverse proxy that forwards the `Host` header. Unexpected `Host`
  values get `421`.
- **systemd.** The unit restricts system calls, address families, namespaces,
  devices and kernel access. Relax one setting at a time with
  `systemctl edit repodeploy` if a site-specific setup needs it.

## Backup, update and removal

```sh
sudo -u repodeploy repodeploy backup --output /secure/path/state.tar.gz
```

The backup contains a consistent copy of the database plus the state files. It
leaves out release checkouts, which can be rebuilt from Git. To update, stop the
service, back up, install the new binary with `scripts/install.sh`, then start
it again. Full procedures, restore and uninstall steps:
[docs/installation.md](docs/installation.md#backup-and-update).

## Upgrading from earlier versions

RepoDeploy was previously named **Pushvori**. Existing installations keep their
data and credentials; see the
[rename upgrade guide](docs/installation.md#upgrade-from-pushvori).

These changes alter behavior. Review them before upgrading:

- **Dockerfile-mode ports bind to `127.0.0.1`.** Applications that were reachable
  on every interface are reachable only through a local reverse proxy after
  their next deployment. Set `"bind_address": "0.0.0.0"` (**Publish port on** in
  the panel) to keep the old behavior.
- **Containers get `no-new-privileges` and capped logs.** Images that rely on
  setuid escalation need `"allow_privilege_escalation": true`.
- **Unexpected `Host` headers are rejected with `421`.** Keep
  `proxy_set_header Host $host;`, or list extra names with `--allowed-hosts`.
- **Login throttling is per client.** Behind a reverse proxy, add
  `--trust-proxy` and forward `X-Forwarded-For`.
- **Password hashes** are strengthened automatically on the next sign-in.
- **With `--public-url`**, the session cookie is renamed
  `__Host-repodeploy_session` and HSTS is sent, so everyone signs in again once.
- **Token files:** the CLI refuses token files readable by group or others.
  Fix them with `chmod 600`.
- **systemd unit:** the unit is stricter. Reinstall it with
  `scripts/install.sh`, and review local overrides if the service fails to
  start.
- **Log masking** ignores fragments shorter than 8 characters, and whole values
  shorter than 4.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

**Integration tests.** They are opt-in, and they fail instead of skipping when
requested without their prerequisites.

```sh
# Real Docker builds, failures, recovery and volumes; disposable SSH Git server
REPODEPLOY_DOCKER_TEST=1 REPODEPLOY_SSH_TEST=1 go test ./internal/deploy -count=1 -timeout=10m

# Real Linux daemon, CLI and web login (use your Docker engine's architecture)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -o bin/repodeploy-linux-arm64 ./cmd/repodeploy
REPODEPLOY_DAEMON_TEST=1 REPODEPLOY_LINUX_BINARY="$PWD/bin/repodeploy-linux-arm64" \
  go test ./cmd/repodeploy -run TestLinuxDaemonIntegration -count=1 -timeout=5m
```

Integration tests create randomly named containers, images and volumes, and
remove only their own fixtures.

**Layout.**

| Path | Contents |
| --- | --- |
| `cmd/repodeploy` | Entry point: daemon, CLI and API client |
| `internal/deploy` | Store (SQLite), Git, Docker/Compose, deployment engine, poller |
| `internal/scm` | GitHub, Bitbucket and SSH connections |
| `internal/web` | Web panel, HTTP API, authentication |
| `packaging`, `scripts` | systemd unit and installer |
| `examples` | Example project and application |

## Scope and limitations

RepoDeploy does not include any of the following:
- Kubernetes, or deployments across several hosts.
- A daemon for Windows or macOS.
- Full Compose compatibility (only a validated subset is supported).
- Multiple users or teams.
- A plugin system or billing.
- A built-in TLS proxy.
- Automatic database recovery.
- Zero-downtime deployments.

## Documentation

- [Installation, update and removal](docs/installation.md)
- [Configuration and recovery boundaries](docs/operations.md)
- [Specification](docs/spec.md)
- [Implementation plan](docs/plan.md) and [progress log](docs/progress.md)

## License

[Apache-2.0](LICENSE)
