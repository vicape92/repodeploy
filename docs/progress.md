# Execution ledger

Plan: docs/plan.md. Spec approved 2026-11-07; Go selected explicitly.

- Environment: empty directory, no Git metadata. Work happens in place; no
  existing branch or worktree to isolate. No commits will be made.
- Environment: Go absent from PATH. Install checksum-verified official Go
  1.27.3 under /private/tmp only. Docker client exists; daemon initially absent.
- Scope: implement the approved MVP incrementally, not only a scaffold.
- Task 1: queue/Git tests RED (missing implementation) -> GREEN; real Git
  checkouts, durable deduplication, concurrent claim, snapshot and process lock.
  SSH integration remains scheduled with the isolated integration suite.
- Docker engine now available in OrbStack's Linux VM. No unrelated containers
  or services are used by tests.
- Dependency decision: add a YAML parser to validate Compose before Docker can
  resolve includes, env files, or paths. This is necessary to enforce the stated
  supported subset safely; no Compose implementation library is added.
- Task 2: test-first engine and Compose implementation. Docker integration passed
  real build failure, startup failure, health failure, recovery and volume
  persistence (95.75s initial run). Retained secret/config snapshots copy into
  rollback releases so cleanup cannot remove their dependencies.
- Task 3: signed webhook filtering/deduplication and real-Git polling/backoff tests
  pass. Branch-target deduplication resets after an observed change, allowing a
  force push back to an earlier commit. Late webhooks reconcile the current head
  before building; superseded jobs fail without activation. This clarifies the
  original spec's deduplication policy while preserving automatic deployment.
- Task 4: password/session/CSRF and bearer API tests, CLI-through-daemon tests and
  race checks pass. Native daemon runs only on Linux. Actual Linux-container
  daemon integration passed CLI, web login, background work and restart (15.46s).
- Integration: isolated SSH fixture passed read-only repository access, checkout,
  rejection of an unauthorized key and strict unknown-host rejection (8.36s).
- Independent review used exactly the principal's inherited model and effort,
  without escalation. Six important findings were reproduced with failing tests
  and fixed: failed-recovery retention, blocked rollback preparation, image VOLUME
  declarations, queued rollback retention, late webhooks, internal timeout retry.
  Additional regressions cover multiline secret filtering and unsupported newer
  database schemas. No reviewer edits or unrelated changes.
- Operational limits: systemd installation/update/uninstall cannot be exercised
  on this macOS host; the Linux daemon runs in an isolated Docker VM. Installer
  syntax is checked; host-wide installation commands are not executed here.
- Final integrated verification: `go test -race -json ./... -count=1 -timeout=10m`
  with Docker, SSH and daemon integration flags enabled: 65 tests/subtests passed,
  no failures or skips. Docker scenarios include rejecting an image's implicit
  VOLUME declaration before activation. Deployment integration 44.11s; SSH 2.71s;
  real Linux daemon 4.27s. Detailed ephemeral output:
  `/private/tmp/pushvori-final-tests.json`.
- Final engine review: extend stale-target protection from GitHub to all automatic
  work, including polling; `TestSupersededPollingJobCannotDowngradeCurrentBranch`
  reproduced the bug RED, then passed GREEN. This keeps the engine independent
  of the provider. Existing retention-preparation fixture now uses a missing
  branch rather than a stale SHA; assertions and retention contract are unchanged.
- Final checks after that correction: `go test ./...`, `go test -race ./...`,
  `go vet ./...`, `go mod verify`, `gofmt -l cmd internal`, installer `sh -n`, and
  static Linux builds for arm64 and amd64 passed. Ordinary runs intentionally
  skip the three opt-in integrations; the integrated run above enabled all three.
- All five increments delivered. No Git initialization, commits, pushes,
  publication, permanent Go installation, or host-wide service installation.

## Real systemd installation verification — 2026-11-07

- Created the user-requested dedicated OrbStack machine `pushvori-systemd-test`,
  Ubuntu 24.04.5 ARM64, with systemd PID 1. Installed Docker, Compose, Git and
  OpenSSH inside this machine only; copied the workspace and verified its binary
  checksum. The unmodified installer and unit verification passed.
- Initial nested OverlayFS build failed with `operation not permitted`.
  Diagnosed the VM-specific storage restriction and configured this machine's
  Docker to VFS with the classic image store. Repeated deployment succeeded;
  no product source, host Docker configuration or existing services were changed.
- Verified actual password login, authenticated CLI, unauthenticated API 401,
  CSRF rejection, service-account Docker access, ownership and private file modes.
- Deployed examples/app through the installed systemd service and a local private
  Git repository with a restricted read-only SSH key. A fixture-only second commit
  was detected through polling and activated without manual deployment.
- Verified persistence after `systemctl restart pushvori` and a complete
  `orb restart pushvori-systemd-test`. Boot ID and PID 1 start time changed. Project,
  active release/image, history, credential hashes and persistent volume marker
  matched the recorded baseline. Both services are enabled and active, NRestarts=0.
- Post-restart verification initially checked Docker health too early, while HTTP
  was ready but Docker still reported `starting`. The verification fixture now
  waits for health, and the complete persistence check passes.
- Detailed reproducible evidence and the VFS configuration are documented in
  docs/installation.md. Machine remains running with the healthy second version.
  Earlier macOS/systemd verification limitation is resolved for this environment.

## Browser/SSH connections and CI gate

- Approved by the user: browser connect/select repository/select branch; added
  Bitbucket Cloud, graphical SSH and optional CI/CD waiting as user steering.
- Spec and executable acceptance written first. New internal/scm package uses
  existing crypto dependency plus stdlib; no new direct/runtime dependencies.
- Bitbucket adapter and SSH assistant implemented in exclusive files by agents
  using the principal's inherited model/effort. Principal wrote their tests and
  reviewed complete files. GitHub App/JWT adapter and web integration implemented
  by principal. API fixtures test signatures, scopes, rotation, pagination and
  repository binding without authorizing a real user account.
- CI gate RED -> GREEN: waiting persists and releases the global worker, project
  FIFO holds, failure/timeout cannot activate, no-wait and rollback bypass CI.
  Additional regressions cover rollback cancellation, failure retention and
  shutdown during in-flight CI requests. Git credentials are URL-scoped command
  environment configuration and are verified with real Git, never persisted.
- Callback tests cover Strict session cookies being absent on cross-site return,
  Lax browser-bound flow cookies, expiry, replay, Host injection, secret-free pages,
  repository/branch form policy and SSH key generation through the panel.
- Independent review found no additional important reproducible defects beyond
  the regression cases already being fixed. Full suite and race detector passed
  before final integration; final evidence follows below.
- Full integration with race detection passed: 103 tests/subtests, zero skips or
  failures, including real Docker builds/failures, CI gating, SSH and Linux daemon
  recovery. Evidence: `/private/tmp/pushvori-connect-final-tests.json`.
- Live Bitbucket host-key retrieval exposed additional ssh.bitbucket.org entries
  in its official response. A new RED regression reproduced this; the parser now
  retains bitbucket.org keys and ignores that documented alternate hostname.
  Complete unit/race suites, vet, module verification and both static Linux builds
  passed after the correction; Docker integrations preceded this parser-only fix.
- Original test machine no longer exists in OrbStack. Created isolated
  `pushvori-connect-test`, installed the unmodified systemd package and deployed
  the example from a restricted local SSH repository. Login/CSRF, connection
  buttons, GitHub manifest permissions, real official host-key retrieval for both
  providers, private key ownership/modes and CI selectors passed against the
  installed binary. No external account was authorized.
- Full VM restart changed boot ID and PID 1 start time. Active commit/image,
  history, configuration, credential hashes, generated graphical SSH keys and
  persistent volume marker survived. Service enabled/active, NRestarts=0; example
  HTTP/health passed. Root-only reports are stored in the test machine.
- Local SSH tunnel verified: panel localhost:18083 redirects unauthenticated
  requests to login; example localhost:18084 returns `first`. Actual GitHub App
  installation and Bitbucket OAuth consent remain user-operated and unverified
  against real accounts. No commits, pushes or publication performed.

## RepoDeploy rename — 2026-11-07

- User selected RepoDeploy. Renamed product/UI/CLI, GitHub App manifest prefix,
  provider User-Agent, cookies, Go module, command directory, systemd unit,
  generated binaries, test opt-in variables and current installation instructions.
  No dependencies or deployment behavior changes.
- Tests failed first for CLI/UI/manifest identity, fresh/legacy installation and
  active-service replacement. Added default-state selection coverage. Installer
  reuses legacy account/state, disables the old unit and refuses either active
  service. Persisted Docker namespaces and absolute state paths remain unchanged.
- Full race-enabled integration passed: 111 tests/subtests, no failures or skips,
  including Docker, SSH and Linux daemon CLI/web/restart. Evidence:
  `/private/tmp/repodeploy-rename-tests.json`. Vet, module verification, shell
  syntax, gofmt and static Linux arm64/amd64 builds passed.
- Took a fresh pre-upgrade baseline including the latest manual deployment, then
  cold-backed up state and old binary in the isolated test VM. Installed the new
  service from `/opt/repodeploy`, retaining account/state and existing password.
  Login, authenticated CLI, active release/image, two history entries, volume
  marker and 17 credential-file hashes matched after upgrade and VM restart.
- RepoDeploy is enabled/active, NRestarts=0; legacy service disabled. Verified the
  existing localhost:18083 panel shows RepoDeploy and localhost:18084 still serves
  the example. Legacy VM/root directory names are retained deliberately. No
  commits, pushes, packages or external account authorizations performed.

## Usability and security review — 2026-11-07

Four blocks applied in order, each test-first against the existing suite.

- Block 1 (security quick wins): Dockerfile-mode ports publish on `127.0.0.1`
  unless `bind_address` selects another loopback/unspecified address; Compose
  mappings without a host IP log a warning. Every service gets
  `no-new-privileges` (opt-out `allow_privilege_escalation`) and json-file log
  rotation. `Host` validation (421) blocks DNS rebinding; `--allowed-hosts`
  adds names. Login throttling is per client (5/min, escalating lockout to 15 min,
  global 30/min) with `--trust-proxy` for loopback reverse proxies; one client can
  no longer lock the administrator out. Git's `file` transport is test-only.
- Block 2 (credentials): `admin.json` records Argon2id parameters (now 3 passes,
  64 MiB); legacy hashes upgrade on sign-in without ending sessions. `passwd` and
  `rotate-token` (CLI and Security page) write atomically; the daemon reloads the
  file, and a password change signs out all browsers via a new epoch. Generated
  SSH keys are listed with their users, deletable when unused, and swept 24 h
  after creation when no project or pending job uses them.
- Block 3 (forms): validation errors carry their field; the browser form re-renders
  with the submitted values, an alert and the field highlighted/focused. A
  Dockerfile/Compose selector hides and discards the other mode's fields. Restoring
  a release needs an explanatory confirmation page; deploy asks for confirmation.
  Git stderr (redacted, last 3 lines) reaches job errors, logs and poll errors.
- Block 4 (features): project deletion (typed-name confirmation, optional
  volumes, refused while a step runs; `repodeploy delete ID --yes [--volumes]`,
  `DELETE /api/projects/ID`); secrets/configs managed from the project page and
  API; dashboard shows each project's latest deployment and refreshes while work
  is pending; local times; `repodeploy backup` (VACUUM INTO, 0600, no overwrite,
  skips checkouts). CSS/JS moved to embedded `/static/`; CSP drops `unsafe-inline`.
- Verification: full race-enabled suite with Docker, SSH and Linux daemon
  integrations enabled: 149 tests/subtests passed, no failures or skips. New
  Docker integration confirms the injected security/log options and loopback port
  on a real container, and project deletion with and without volumes. Vet, module
  verification, gofmt and installer syntax pass; Linux arm64/amd64 binaries
  rebuilt in `bin/`. No commits, pushes or publication performed.

## Remaining review items — 2026-11-07

- HTTPS (`--public-url`) sends HSTS and uses `__Host-repodeploy_session` and
  `__Secure-repodeploy_connect_*` cookies; loopback HTTP keeps the old names.
- systemd unit gains kernel/device/namespace/address-family/syscall restrictions
  and empty capability sets. `systemd-analyze security`: 9.0 UNSAFE -> 3.4 (offline)
  / 3.6 (loaded) OK. Verified twice in throwaway OrbStack Ubuntu 26.04 VMs
  (systemd 259, Docker 29.1, Compose 2.40) with the current binary: installer,
  start-up checks, SSH Git deploy, polling deploy with `pull_images`, backup,
  CLI without sudo, deletion with volumes, Host validation, CSP; no sandbox
  errors in the journal. VMs deleted afterwards.
- Log masking ignores fragments under 8 characters (whole values from 4).
- `pull_images` pulls image: services and adds `build --pull`; `$$` is accepted
  as Compose's literal-dollar escape (real Compose integration verifies both).
- Webhook bodies up to 25 MiB (GitHub's cap) with at most 4 buffered at once;
  other requests keep 1 MiB.
- CLI: `REPODEPLOY_URL`, `REPODEPLOY_TOKEN_FILE`, `~/.config/repodeploy/token`;
  token files must be 0600.
- Verification: full race-enabled suite with Docker, SSH and Linux daemon
  integrations: 157 tests/subtests passed, no failures or skips. Vet, module
  verification, gofmt and installer syntax pass; Linux binaries rebuilt.
  No commits, pushes or publication performed.

## Environment variables and manual upload — 2026-11-08

- Modular monolith: new `internal/envvars` and `internal/upload` modules
  (stdlib only); engine selects sources through `SourcePreparer`.
- Variables: storage, `.env` import, restricted Compose substitution,
  build-time opt-in, digest per release, deploy changes, rollback restores
  values, log redaction; API, panel and CLI.
- Uploads: validated and normalised archives, retention with a one-hour grace,
  backups, panel form, `POST /api/projects/{id}/uploads`, `repodeploy up`.
- Verification: gofmt and `go vet` clean; `go mod verify` all modules verified;
  `go test ./... -count=1` and `go test -race ./... -count=1` ok for every
  package; linux/amd64 and linux/arm64 builds and `sh -n scripts/install.sh`
  pass. The Docker integration `TestDockerIntegrationEnvironmentAndUpload` ran
  (not skipped) and passed for the dockerfile and compose subtests (about 50 s),
  and the full `internal/deploy` suite with `REPODEPLOY_DOCKER_TEST=1` passed
  (about 123 s).

## Security and concurrency corrections — 2026-11-08

Spec: credential updates are serialized across processes, password epochs bind
sessions, slow bodies expire before occupying password verification slots, and
IPv6 clients share a /64 login budget. Aggregate password work stays bounded
without a global minute-long login lockout or an unbounded client table.
Backups pin referenced artifacts; project deletion, archive/key collection and
mutations cannot race. Existing project updates never recreate deleted rows.
Logs redact the actual release inputs, read incrementally and retain a bounded
writer. CI and repository polling have deadlines and bounded concurrency; SCM
network calls do not retain the configuration mutex or resurrect disconnected
credentials. Editing a detection source resets its backoff; stale observations
are discarded. SSH creation and collection participate in backup exclusion. Queue/history queries use indexes. Retained releases deliberately
keep their protected secret snapshots for rollback; recovery is tested across
intermediate state rather than changing its contract.

Acceptance: deterministic regression tests for these interleavings and resource
limits fail before implementation and pass afterwards; existing behavior,
race-enabled tests, vet and applicable Docker/SSH/daemon integrations pass.
No new external dependencies, commits or pushes.

Plan (TDD; work in the requested checkout):
- [x] Credentials, session epochs, HTTP read deadlines and bounded login limits.
- [x] SCM cache synchronization, cancellable network work and parallel polling.
- [x] Artifact pinning, project mutation exclusion, archive/SSH collection.
- [x] Release-specific log masking, incremental reads and persistent writers.
- [x] CI isolation, database indexes and crash-recovery regression coverage.
- [x] Review the complete diff and run integration, race, vet and build checks.

Verification: full race-enabled suite with Docker, SSH and Linux daemon
integrations: 266 tests/subtests passed, no failures or skips. The final partial
queue-index and oversized-log-line redaction regressions also passed with the
race detector after their addition.
Vet, module verification, gofmt, installer syntax, and Linux arm64/amd64 builds
passed. Backups briefly exclude artifact mutations while copying; live
applications keep serving. No commits or pushes performed.
