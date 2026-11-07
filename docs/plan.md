# RepoDeploy MVP implementation plan

## RepoDeploy rename implementation

Spec: docs/spec.md, RepoDeploy rename. No commits or pushes.

- [x] Add failing CLI/UI/manifest/default-state and installer compatibility tests.
- [x] Rename public surfaces, Go module/command and service; keep persisted Docker
  identities, reuse legacy paths/account and update installation documentation.
- [x] Run Go/race/vet/build checks; upgrade the isolated OrbStack installation with
  backup and verify the renamed panel, credentials, history and persistent volume.

Goal: install a single-host daemon, connect Git, and deploy healthy exact-commit
Docker releases through one engine shared by CLI, web, polling, and GitHub.

Architecture: one Go executable, SQLite, one durable worker, external Git/SSH
and Docker Compose. HTML templates and SSE; no frontend build toolchain.
Spec: [spec.md](spec.md).

## Constraints

English code and identifiers. Linux runtime. No shared checkout, SaaS, global
Docker cleanup, secret logging, data rollback promise, commits or publishing.
Minimal dependencies: SQLite driver, password hashing, and YAML parser (needed
to validate Compose before resolving files or external configuration).

## Review focus

- Crash between a Docker side effect and its database update.
- Duplicate events, repeated commits, and cross-process workers.
- Manifest paths, mount paths, service changes and mutable image tags.
- Credentials in subprocess output, persisted configuration, and logs.
- Rollback failure and automatic redeployment of a rejected release.

## Tasks

- [x] 1. Queue and Git: internal/deploy/{store,git}.go and matching tests.
  Persist projects/deployments, transactional claims and deduplication. Resolve
  and persist SHA, prepare fresh detached checkouts, strict SSH host keys.
  RED: concurrent enqueue/claim, reopen database, branch advance, dirty checkout,
  Git error and credential handling. GREEN: minimal implementation. Verify
  go test ./internal/deploy and repeat with -race.
- [x] 2. Deployment and recovery: internal/deploy/{engine,docker}.go and tests.
  Persist phase intent, retain immutable manifests/images, run health checks,
  rollback, pause/block, retention. RED: external runtime failure at each phase,
  restart after each phase and first-deploy failure. GREEN: minimal engine and
  Compose adapter. Verify complete suite and isolated Docker integration.
- [x] 3. Detection: internal/deploy/detect.go and tests. RED: HMAC/repository/ref,
  duplicate persistence, poll advances/errors/backoff. GREEN: adapters enqueue
  through the same durable store. Verify full suite with race detector.
- [x] 4. API, CLI and web: internal/web and cmd/repodeploy. RED: unauthenticated
  access, browser CSRF, project validation, deploy/rollback/history/logs. GREEN:
  authenticated server, HTML UI, SSE, CLI client and daemon lifecycle. Verify
  full suite, vet and Linux compilation.
- [x] 5. Operations: README, installation guide, systemd unit, sample Docker app,
  LICENSE, integration tests. Verify sample with build/start/health failures,
  recovery and volume persistence on isolated Linux Docker resources. Record
  any unavailable prerequisites accurately. Review all final files.

Each task follows failing tests -> minimal implementation -> green tests ->
refactor. Continue in this chat; do not request another approval between tasks.
Progress and verification evidence are recorded in docs/progress.md.

## Browser and SSH connection increment

1. Test-first provider credential storage and adapters in internal/scm. GitHub
   manifest/JWT/installation tokens; Bitbucket OAuth/rotation and repository lists.
   GitHub and Bitbucket implementations own separate files with shared fixed types.
2. Test-first Git credentials and branch listing, preserving exact checkout and
   existing public/SSH behavior. Engine and poller share the credential manager.
3. Test-first authenticated web connection routes, callback browser state, provider
   repository/branch selection, SSH assistant and create/deploy form. No credentials
   in HTML or project JSON. Preserve manual/CLI configuration.
4. Full tests/race/vet/Linux build; security review, provider API fixture smoke
   tests and updated docs. Update an available isolated test instance; account
   authorization is performed by the user, never silently by the agent.
5. Test-first optional CI gate: provider adapters aggregate statuses, queue parks
   `waiting_ci` without blocking other work, timeout/restart/failure tests, and a
   graphical per-project setting. Existing default behavior remains immediate.
