# RepoDeploy MVP specification

## RepoDeploy rename

- Product/UI name: RepoDeploy; Go module, executable, command directory and new
  systemd service: repodeploy. New installations use /var/lib/repodeploy and the
  repodeploy service account. No new dependencies or deployment behavior changes.
- Existing /var/lib/pushvori installations retain their absolute state/key/release
  paths and account. The CLI selects existing new state first, then legacy state.
  The installer refuses to replace either running service, reuses legacy state
  when appropriate and disables the old service before enabling the renamed one.
- Existing Docker project/volume/image namespaces remain stable for rollback and
  retention. Existing credentials, projects, history and provider registrations
  are preserved. Browser sessions require a new login after restart.
- Acceptance: new CLI/UI/manifest branding, fresh and legacy installer tests,
  active-service guard, complete Go checks and a live systemd upgrade preserving
  the example deployment, credential hashes and volume marker.

Approved on 2026-11-07. Implementation language: Go.

## Product

Self-hosted deployment automation for one Linux host and one administrator.
Git and Docker Engine with Compose v2 are prerequisites. No SaaS, hosted CI,
registry account, Kubernetes, multi-host orchestration, or plugin architecture.
The same daemon serves the web interface and CLI API.

## Inputs and outputs

A project specifies a Git URL, deployment branch, optional read-only SSH key
and known-hosts file, Dockerfile or Compose path, health check, detection mode
(manual, polling, GitHub webhook), polling interval, and release retention.
Output: durable deployment ID, exact commit, timestamps, duration, state, logs,
error, recovery result, and separately tracked active release.

States: queued, preparing, building, activating, checking, succeeded, failed.
Recovery is recorded separately; restoring an old release does not turn a
failed deployment into a successful one. A failed first deployment has no
previous release to restore.

## Execution contract

One global worker and persistent FIFO queue. A process lock excludes a second
daemon for the same state directory. Duplicate deliveries and automatic
requests for an already observed commit do not schedule duplicate deployments.
Commit deduplication applies while the observed branch target is unchanged;
moving away and returning to an older commit is a new change. Before building,
superseded automatic requests reconcile the latest branch head without activating
the stale commit. This avoids downgrades from out-of-order delivery.
Manual requests can retry a commit. SHA is resolved and persisted before build;
each execution has a fresh detached checkout. No mutable checkout is shared.

Build and image preparation cannot replace running containers. Activation uses
a stable Compose project name and retained, immutable image IDs. An explicit
health check is required. Failure during activation or health triggers recovery
of the prior manifest and its health check. Recovery failure blocks automation
for that project and remains visible. Manual rollback pauses automation until
the administrator explicitly resumes it. No zero-downtime or data rollback
guarantee. Existing volumes and external configuration must survive.

Record intent before Docker side effects. On restart, reconcile interrupted
activation by restoring the recorded prior release (or cleaning up a failed
first activation); interrupted preparation/build can restart from a clean
checkout with its persisted commit. Only start new work after reconciliation.

## Supported Docker configuration

Dockerfile mode generates Compose configuration. Compose mode accepts one
file, local build contexts within the checkout, explicit persistent named
volumes, external secret/config files, and stable service topology. Reject
privileged containers, host networking, container_name, Docker socket mounts,
anonymous volumes, mutable source mounts, remote build contexts, includes,
and unsupported constructs with actionable errors. Do not claim arbitrary
Compose compatibility. Retain the effective manifest and image identities.
No global Docker prune or volume deletion.

## Detection

GitHub push events: HMAC-SHA256 over the original bytes, constant-time
comparison, repository/branch checks, durable delivery deduplication, then a
quick 202 response. Execute asynchronously. Deleted branches do not deploy.
Unreachable commits fail explicitly. Polling observes the latest branch SHA
at a configurable interval without inbound ports; intermediate commits can be
missed. Network errors preserve the active version and back off. Webhooks may
need manual redelivery after outages; polling reconciliation is optional.

## Security and operations

Listen on loopback by default. Administrator authentication, protected sessions,
CSRF protection for browser writes, and CLI bearer authentication. Remote panel
and webhook exposure require an HTTPS reverse proxy. SSH keys and secrets live
outside repositories, with restricted filesystem permissions and strict host
key verification. Do not log secrets or pass them in command-line arguments.
Only trusted repositories: Docker control is effectively host administrator
access, and repository code can disclose any application secret it receives.

SQLite stores configuration, queue, and history. Logs and artifacts are local,
bounded, and protected. Retain five releases by default, configurable; always
protect active and recovery releases. Installation and upgrade are reproducible
and documented. Back up state before schema changes. Uninstall preserves data
unless explicitly removed. Proposed license: Apache-2.0.

## Delivery and executable acceptance

1. Durable queue and Git preparation: duplicate/concurrent requests produce one
   job; only one claim executes; an advancing branch cannot change a pinned SHA;
   checkouts are isolated; Git failures hide credentials; restart cannot reuse a
   partial checkout. Real Git and isolated SSH tests cover access and host keys.
2. Docker deployment: sample app serves its commit; build failure leaves the
   active container untouched; startup/health failure restores the last healthy
   image; failed recovery blocks subsequent work; volumes survive rollback;
   engine termination at each phase has a deterministic recovery path.
3. Detection/API/web: invalid signatures, wrong branches, duplicate deliveries,
   polling errors, unauthorized requests, CSRF, manual deploy and rollback are
   covered. CLI and web invoke the same queue and expose history and live logs.
4. Operations: documented installation, update, uninstall, complete Docker
   sample and dedicated integration suite. Tests never remove unrelated Docker
   resources. Linux is the supported runtime; tests may run on other hosts.

All behavior is developed test-first. Required checks: go test ./..., go test
-race ./..., go vet ./..., Linux build, and the isolated integration suite when
Docker and SSH server prerequisites are available. No commits, pushes, package
publishing, or edits to unrelated services.

## Browser and SSH repository connections

Approved flow: connect provider -> authorize in browser -> select repository and
branch -> deploy. Providers: github.com and Bitbucket Cloud (bitbucket.org).
Keep URL/SSH configuration working. Add a graphical SSH assistant that generates
a key, shows its public part, verifies access and lists branches without a terminal.

GitHub: register this self-hosted instance through an App manifest, then install
the App on selected repositories. Request only contents/metadata read. Bitbucket:
one-time OAuth consumer setup in the panel (account read and repository read),
then browser authorization with automatic rotating refresh tokens. No central
SaaS, shared vendor app, new dependency, or credentials in clone URLs. Initial
provider app registration is an unavoidable provider requirement and is explained
in the UI. Local setups use polling; Bitbucket webhooks/Data Center are excluded.

Callbacks use single-use, expiring state bound to the administrator's browser
through a SameSite=Lax flow cookie. Keep normal administrator cookies Strict;
cross-site callback verification cannot rely on the Strict cookie being sent.
Only configured HTTPS origins or loopback HTTP origins may receive credentials.
Persist provider secrets atomically with mode 0600 outside checkouts. Renew tokens
server-side, scope GitHub installation tokens to the selected repository, and
never expose tokens to pages, job snapshots, Git arguments, Git config or builds.
Git credentials travel in command-scoped environment configuration, with redirects
disabled. API pagination cannot forward authorization to another host. Revocation,
expired state and restart during authorization must yield actionable reconnect
errors without affecting the active application.

SSH wizard supports GitHub/Bitbucket host trust from their official HTTPS sources,
not unauthenticated keyscan. Keys remain private on the host. Users register only
the public key in repository settings; the assistant verifies before creating a
project. Existing custom SSH paths remain available as an advanced option.

Acceptance: authenticated browser buttons; state/cookie/expiry/replay rejection;
both providers list permitted repositories and branches; token rotation and
restart persistence; wrong repository/host cannot obtain credentials; private
Git HTTP checkout is exact and credential-free on disk; SSH public-key workflow;
existing deploy/recovery tests remain green. Provider API fixtures exercise real
HTTP, RSA signatures and persistence; live account consent remains user-operated.

Projects may deploy immediately (default) or wait for provider CI/CD results for
the pinned commit. GitHub uses commit statuses and latest check runs; Bitbucket
uses commit build statuses. All published results must pass; absent/pending results
wait, failed/cancelled results fail the deployment. Timeout defaults to 30 minutes
and is configurable. `waiting_ci` is durable, releases the global worker, survives
restart, and rechecks without building or activating. Manual rollback bypasses CI
because it restores an already verified retained release. Provider API access is
required to inspect private CI even when Git transport uses SSH; the UI explains
and validates that requirement rather than silently bypassing CI. A waiting job
holds later deployments of the same project in FIFO order while other projects
continue. Superseded automatic targets are failed and the current head reconciled.
