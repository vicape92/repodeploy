# RepoDeploy

Previously named Pushvori. Existing installations can keep their data and
credentials: follow the [rename upgrade guide](docs/installation.md#upgrade-from-pushvori).

Self-hosted Git-to-Docker deployments for one Linux host and one administrator.
No GitHub Actions, SaaS account, hosted database, or mandatory external registry.
Repository access and image downloads still require network connectivity.

**Status:** initial MVP. Run only trusted repositories on a host you administer.
Docker access normally grants root-equivalent control of that host.

## What it does

- Public HTTPS and private SSH repositories; one branch per project.
- GitHub signed push webhooks, outbound polling, and manual deployments.
- Browser connections for GitHub Apps and Bitbucket Cloud OAuth, plus a graphical
  SSH key assistant. No GitHub CLI or terminal key handling is needed.
- Optional waiting for commit CI/CD statuses before building or activating.
- One durable deployment worker shared by the web interface and CLI.
- Exact-commit, isolated checkouts; Dockerfile or a validated Compose subset.
- Build while the current version runs; health checks and automatic recovery.
- Retained release history and logs, confirmed manual rollback, restart reconciliation.
- Project deletion, managed Compose secrets from the panel, live dashboard status.
- Consistent online backups of the state directory.
- Password-protected web interface and authenticated CLI API: per-client login
  throttling, `Host` validation against DNS rebinding, a strict
  Content-Security-Policy, password change and CLI token rotation.
- Containers run with `no-new-privileges` and rotated logs; Dockerfile-mode ports
  are published on `127.0.0.1` unless configured otherwise.

Activation can interrupt traffic. Restoring containers **does not undo database
migrations or writes**. Failed recovery blocks the project for intervention.
Rolling back manually pauses automatic deployments until explicitly resumed.

## Quick start

Requires Linux with systemd, Git, OpenSSH client, Docker Engine and Compose v2
(2.24+; later major versions are accepted). Build with Go 1.27.3:

```sh
go mod download
go test ./...
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o bin/repodeploy ./cmd/repodeploy
sudo sh scripts/install.sh bin/repodeploy
```

Create a password file outside the repository, mode `0600`, containing at least
12 characters. Initialize as the service user, then start the daemon:

```sh
sudo -u repodeploy /usr/local/bin/repodeploy init --password-file - < /secure/path/admin-password
sudo systemctl enable --now repodeploy
```

Open `http://127.0.0.1:8081` on the host, or use an SSH tunnel:

```sh
ssh -L 8081:127.0.0.1:8081 your-server
```

In the panel, choose **Connect GitHub, Bitbucket or SSH**. Authorize the provider,
choose a repository and branch, configure its Docker/health settings, and select
**Deploy immediately** or **Wait for CI/CD**. See the
[connection setup guide](docs/installation.md#browser-and-ssh-connections).
Alternatively, edit
[`examples/project.json`](examples/project.json), then run:

```sh
sudo -u repodeploy repodeploy add --file /absolute/path/project.json
sudo -u repodeploy repodeploy projects
sudo -u repodeploy repodeploy deploy PROJECT_ID
sudo -u repodeploy repodeploy history PROJECT_ID
sudo -u repodeploy repodeploy logs JOB_ID --follow
sudo -u repodeploy repodeploy rollback PROJECT_ID RELEASE_ID
sudo -u repodeploy repodeploy resume PROJECT_ID
sudo -u repodeploy repodeploy delete PROJECT_ID --yes [--volumes]
sudo -u repodeploy repodeploy backup --output /secure/path/state.tar.gz
sudo -u repodeploy repodeploy passwd --password-file - < /secure/path/new-password
sudo -u repodeploy repodeploy rotate-token
```

Global flags precede the command. `--state` selects a different state directory;
`--url` and `--token-file` (or `REPODEPLOY_URL` and `REPODEPLOY_TOKEN_FILE`)
configure a remote CLI. Remote access requires HTTPS. A private copy of the token
in `~/.config/repodeploy/token` lets administrators skip `sudo`; see
[using the CLI without sudo](docs/installation.md#using-the-cli-without-sudo).

### Upgrading from earlier versions

These changes alter behavior; review them before upgrading:

- **Dockerfile-mode ports bind to `127.0.0.1`.** Applications previously reachable
  on every interface are reachable only through a local reverse proxy after their
  next deployment. Set `"bind_address": "0.0.0.0"` (or **Publish port on** in the
  panel) to keep the old behavior.
- **Containers get `no-new-privileges` and capped logs.** Images that rely on
  setuid escalation need `"allow_privilege_escalation": true`.
- **Unexpected `Host` headers are rejected** with `421`. Keep
  `proxy_set_header Host $host;`, or list extra names with `--allowed-hosts`.
- **Login throttling is per client.** Behind a reverse proxy, add `--trust-proxy`
  and forward `X-Forwarded-For`; see
  [HTTPS exposure](docs/installation.md#https-reverse-proxy).
- Password hashes are strengthened automatically on the next sign-in.
- With `--public-url`, the session cookie is renamed `__Host-repodeploy_session`
  and HSTS is sent; everyone signs in again once after the upgrade.
- The CLI refuses token files readable by group or others (`chmod 600`).
- The systemd unit is stricter. Reinstall it with `scripts/install.sh`; review
  local overrides if the service fails to start.
- Log masking ignores fragments shorter than 8 characters (whole values from 4).

## Configuration and examples

[`examples/app`](examples/app) contains a complete BusyBox HTTP application with
a Dockerfile, Compose healthcheck, persistent named volume, and failure switches.
Copy those files to the root of your own Git repository. The default example
publishes port `18080` on `127.0.0.1`; change it if occupied. Set the project Compose path to
`compose.yaml`, health service to `app`, and leave Dockerfile/HTTP health URL empty.

For Dockerfile mode, set the Dockerfile path, host/container ports and an HTTP
health URL on the host's loopback address. The host port is published on
`127.0.0.1`; select `0.0.0.0` under **Publish port on** to expose it directly. Paths are relative to repository root.

For private repositories, browser authorization manages credentials automatically.
The SSH assistant generates a private key on the host, shows only the public key,
links to repository access-key settings and checks access before listing branches.
For advanced SSH setups, provision an unencrypted, repository-scoped read-only
deploy key and a **verified** `known_hosts` file readable by the service user.
Enter their absolute paths. The engine disables the SSH agent, password prompts,
and automatic trust of unknown hosts. Submodules and Git LFS are not supported.

GitHub webhooks are configured manually in the repository settings:

1. Select webhook mode, or webhook + polling reconciliation, for the project.
2. Generate a webhook secret in the UI or `repodeploy webhook-secret PROJECT_ID`.
3. Use `https://your-host/hooks/github/PROJECT_ID`, JSON, push events only,
   that secret, and SSL verification enabled.

The handler persists work before returning `202`; builds run asynchronously.
See [installation and HTTPS exposure](docs/installation.md) and
[supported configuration and failure behavior](docs/operations.md).

## Tests

```sh
go test ./...
go test -race ./...
go vet ./...
REPODEPLOY_DOCKER_TEST=1 REPODEPLOY_SSH_TEST=1 go test ./internal/deploy -count=1 -timeout=10m
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -o bin/repodeploy-linux-arm64 ./cmd/repodeploy
REPODEPLOY_DAEMON_TEST=1 REPODEPLOY_LINUX_BINARY="$PWD/bin/repodeploy-linux-arm64" go test ./cmd/repodeploy -run TestLinuxDaemonIntegration -count=1 -timeout=5m
```

Integration tests create randomly named, project-owned containers, images and
volumes. They remove only their own fixtures. Docker integration deliberately
fails builds, startup and health checks, verifies recovery, and checks persisted
data. SSH integration runs a disposable read-only Git server. Process-death and
restart recovery tests run in the ordinary suite. Docker/SSH integration skips
unless explicitly enabled; requested integrations fail if prerequisites are absent.
Use the Linux architecture of your Docker engine (`amd64` or `arm64`) for the
daemon integration binary. That test checks the real Linux daemon, authenticated
CLI, web login, background failure logging and configuration after restart.

## Scope

No Kubernetes, multi-host deployment, Windows/macOS daemon support, arbitrary
Compose compatibility, user teams, plugin system, billing, built-in TLS proxy,
automatic database recovery, or zero-downtime guarantee.

Specification: [docs/spec.md](docs/spec.md). Implementation plan and verification:
[docs/plan.md](docs/plan.md), [docs/progress.md](docs/progress.md).

License: [Apache-2.0](LICENSE).
