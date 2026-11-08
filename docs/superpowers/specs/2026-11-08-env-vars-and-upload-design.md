# Environment variables and manual upload — design

Approved 2026-11-08. Implementation language: Go. Extends `docs/spec.md`.

## Goal

RepoDeploy aims to be a self-hosted, single-host Railway: install it on a
personal machine, VPS or cloud server, then deploy with little effort. Two gaps
block that today:

1. **Per-project environment variables.** Values such as `DATABASE_URL` must
   not live in Git. Today the only options are file-based Compose
   `secrets:`/`configs:` or literal values committed in `environment:`.
2. **Manual upload.** A project without a Git repository cannot be deployed.
   Users must be able to upload a `.zip`/`.tar.gz` from the panel or push a
   local folder from the CLI.

Both reuse the existing engine guarantees: durable FIFO queue, immutable
releases, explicit health check, automatic recovery, manual rollback,
retention, restart reconciliation.

## Out of scope

Automatic domains/HTTPS, managed databases, variables shared across projects,
per-service variable assignment in the panel, uploads into Git projects,
schema migrations.

## Architecture — modular monolith

RepoDeploy stays one binary and one daemon, organised as modules with explicit
boundaries. Each new capability is its own package that owns its data and
exposes a small API; no other package reads or writes that data directly.

| Module | Owns | Depends on |
|---|---|---|
| `internal/envvars` | `secrets/<id>.env.json`; validation, `.env` parsing, digest, `${VAR}` substitution | stdlib only |
| `internal/upload` | `uploads/`; safe extraction, normalisation, hashing, retention, client-side file selection and packing | stdlib only |
| `internal/deploy` | queue, engine, Docker runtime, Git source (unchanged structure) | `envvars`, `upload`, `scm` |
| `internal/web` | HTTP API and panel; new handlers in `env.go` and `uploads.go` | `deploy`, `envvars`, `upload`, `scm` |
| `cmd/repodeploy` | CLI and daemon wiring; `up` in `up.go` | all of the above |

- `deploy.Store` exposes the module stores as fields (`Env *envvars.Store`,
  `Uploads *upload.Store`), following the existing `Sources *scm.Manager`
  pattern.
- The engine prepares source code through a `SourcePreparer` interface
  (`Prepare(ctx, project, ref, dest)`), implemented by the Git source and by an
  adapter over `upload.Store`. Project deletion asks each module to delete its
  own data (`envvars.DeleteProject`, `upload.DeleteProject`).
- No import cycles: `envvars` and `upload` never import `deploy` or `web`.
- Restructuring the rest of `internal/deploy` into modules is a separate,
  later project and out of scope here.

## Part 1 — Environment variables

### Storage

- One file per project: `<state>/secrets/<project-id>.env.json`, mode 0600,
  written atomically (temp file + rename), same protection as managed secrets.
  `repodeploy backup` already archives `secrets/`, so variables are backed up.
- Content: ordered list of `{"name": string, "value": string, "build": bool}`.
- Names match `^[A-Za-z_][A-Za-z0-9_]*$`, at most 100 characters, unique per
  project. Values are arbitrary UTF-8, at most 64 KiB each; at most 500
  variables per project.
- Deleting a project deletes its variables file.

### Injection (inside `loadCompose`)

- **Dockerfile mode:** every variable is added to the generated `app` service
  `environment:` mapping. Variables with `build: true` are also added to
  `build.args`.
- **Compose mode:** RepoDeploy itself performs substitution, before validation,
  and only in:
  - values of a service's `environment:` (mapping values, or the value part of
    `KEY=value` list entries);
  - values of a service's `build.args:` mapping.

  Supported syntax: `${NAME}` and `${NAME:-default}`. `$$` remains a literal
  `$`. Any other `$` — elsewhere in the file, in keys, or with other syntax
  (`$NAME`, `${NAME?err}`, `${NAME-x}`) — keeps today's rejection, with an error
  message naming where interpolation is allowed. This keeps variables from
  influencing images, volumes, ports, privileges or any validated field.
- A reference in `build.args` to a variable without `build: true` fails with an
  error explaining that build arguments are stored in image history and how to
  enable the flag. A `:-default` value used in `build.args` is allowed.
- A referenced variable that is undefined and has no default fails the
  deployment before the build, listing every missing name. The active release
  is untouched.
- Substituted values are written into the release's `build.json` and
  `compose.json` (both 0600 inside `releases/<job>/`). A release is therefore
  self-contained: rollback restores the variables that release ran with,
  through the existing `copyArtifact` path, without extra logic.

### Pending changes

- Each job snapshot records `env_digest`: SHA-256 over the canonical JSON of the
  variables (sorted by name, including value and build flag) used by that
  build.
- The project is in "undeployed changes" state when it has an active release
  and the current digest differs from the active release's digest. A release
  without `env_digest` (created before this feature) counts as the digest of
  an empty variable set.
- **Deploy changes** enqueues a manual job pinned to the active release's
  source — the same Git commit, or the same upload hash — so it rebuilds with
  the current variables. It behaves exactly like a manual deployment of that
  commit: build, activation, health check and recovery; if the project waits
  for CI, the commit's results are already published, so the wait ends at the
  first check. Unavailable (API 409, no button) when there is no active
  release; the first deployment already uses the stored variables.
- Saving variables never triggers a deployment by itself.

### Interfaces

- **Panel (project page):** variables table with masked values, a per-row
  **Show** button (POST with CSRF; the value is shown only on that response
  page), delete, an add/replace form (saving an existing name replaces its
  value and flag) with a build-time checkbox and warning, and a **Paste .env** mode
  that parses `KEY=value` lines (supports `#` comments, `export ` prefix, single-
  and double-quoted values with `\n` escapes in double quotes) and upserts them.
  The "Undeployed changes" banner and **Deploy changes** button appear when
  pending.
- **API (bearer):**
  - `GET /api/projects/ID/env` → names, build flags, pending state; values only
    with `?reveal=1`.
  - `PUT /api/projects/ID/env` → upsert a list of variables.
  - `DELETE /api/projects/ID/env/NAME`.
  - `POST /api/projects/ID/deploy-changes`.
- **CLI:** `repodeploy env list ID [--reveal]`, `repodeploy env set ID NAME
  [--build]` (value read from stdin, never from argv), `repodeploy env unset ID
  NAME`, `repodeploy env import ID FILE` (`-` for stdin), and `repodeploy
  deploy-changes ID`.
- **Logs:** variable values join the existing secret masking in `LogWriter`
  with the same length thresholds.

## Part 2 — Manual upload

### Model

- `Project.Source` is `"git"` (default when absent, so existing projects are
  unchanged) or `"upload"`. It is chosen at creation and cannot be changed
  later.
- Upload projects reject repo, branch, connection, repository ID, SSH key,
  known hosts, polling, webhook and CI fields. Mode, Dockerfile/Compose path,
  ports, health check, retention and variables work as today.
- Job identity: the upload's content hash is stored in the existing `sha`
  column; jobs created by an upload have `source="upload"`. The API exposes the
  hash as `upload` for upload projects, and history shows `upload <first 12 hex>`
  where Git projects show the commit. No SQLite schema change; `user_version`
  stays 1.

### Receiving an upload

Endpoints: `POST /api/projects/ID/uploads` (bearer; raw archive body,
`Content-Type: application/octet-stream`) and `POST /projects/ID/uploads`, the
panel's multipart form (field `file`, session + CSRF). Steps:

1. Stream the body into a temp file under `<state>/uploads/tmp/` (0600). Limit:
   200 MB by default, configurable with the daemon flag `--max-upload`
   (bytes, accepts `MB`/`GB` suffixes). Exceeding it returns 413 and deletes the
   temp file. Upload routes are exempt from the 1 MiB default request cap and,
   like webhooks, at most 2 uploads are processed concurrently (429 otherwise).
2. Detect the format by magic bytes: ZIP (`PK\x03\x04`) or gzip-compressed tar.
   Anything else returns 400.
3. Safe extraction into a fresh temp directory:
   - reject absolute paths, `..` components, entries that resolve outside the
     root, hardlinks, device files, FIFOs and sockets;
   - symlinks are allowed only if their target resolves inside the root;
   - limits: total uncompressed size at most 5× `--max-upload` (1 GB with the
     default), at most 50 000 entries; exceeding either returns 400 (zip-bomb
     protection);
   - regular files keep only the executable bit (0755 or 0644); directories 0755.
4. If the extracted root contains exactly one directory and nothing else,
   flatten it (archives made by "compress folder" on macOS/Windows). `__MACOSX/`
   and `.DS_Store` entries are ignored.
5. Normalize: re-pack as tar.gz with entries sorted by path, mtime 0, uid/gid 0,
   no user/group names, and fixed gzip header. The SHA-256 of that normalized
   archive is the upload identity.
6. Move it atomically to `<state>/uploads/<project-id>/<sha256>.tar.gz` (0600)
   **before** enqueuing, so restart during preparing/building can re-extract.
7. Deduplication: if the hash equals the active release's hash, return 200 with
   the existing job and enqueue nothing, unless `force=1` is set (CLI
   `--force`). Otherwise enqueue and return 202 with the job ID.

Validation errors never leave files behind.

### Engine

- `execute` calls `prepareSource(ctx, job, dest)` in the `preparing` phase:
  `Git.Prepare` for Git projects, `Upload.Prepare` for upload projects
  (extract the retained archive into `releases/<job>/source`, re-applying the
  same safety checks).
- `Resolve`, superseded-target reconciliation and CI waiting apply only to Git
  projects. Build, activation, health check, recovery and rollback are
  unchanged.
- A missing retained archive fails the job with "upload archive unavailable;
  upload it again".

### Retention

An archive is deleted during `prune` when no retained job row (history,
queued or running) references its hash. Active and recovery releases stay
protected as today, so their archives do too. Archives modified in the last
hour are never pruned, which closes the window between storing an upload and
enqueuing its job. Project deletion removes `uploads/<project-id>/`. Temp files
older than 24 hours in `uploads/tmp/` are swept at daemon start. `repodeploy
backup` includes `uploads/` except `uploads/tmp/`.

### CLI `repodeploy up [--project ID] [DIR]`

- `DIR` defaults to the current directory. `--project` may be omitted when
  `DIR/.repodeploy/project` contains the ID; a successful upload with
  `--project` writes that file.
- File selection:
  - if `DIR` is inside a Git work tree, use `git ls-files -co --exclude-standard`
    (exactly respects `.gitignore`);
  - otherwise include everything except `.git/`;
  - in both cases apply `.repodeployignore` (gitignore subset: `#` comments,
    globs with `*`, `?`, `**`, trailing `/` for directories, leading `/` to
    anchor, `!` negation);
  - always exclude `.git/`, `.repodeploy/` and files matching `.env` or `.env.*`,
    printing a notice that suggests `repodeploy env import`.
- Packs to a temp tar.gz (with the same symlink rules), reports the server's
  limit clearly if the upload is rejected with 413, uploads with the bearer
  token, prints the job ID, then follows logs until the job finishes and exits
  non-zero on failure. `--detach` returns right after enqueuing.
- Works from a laptop through `REPODEPLOY_URL` + token when the panel is behind
  an HTTPS reverse proxy (existing remote-access model).

### Panel

- New-project form: a first choice between "Git repository" and "Manual
  upload"; the upload choice hides the Git fields.
- Upload project page: file picker (`.zip`, `.tar.gz`, `.tgz`) with size limit
  shown, upload button, and history entries labeled with the upload hash.

## Errors shown to users

| Situation | Result |
|---|---|
| Referenced variable undefined | Deployment fails before build listing missing names; active release untouched |
| `${VAR}` outside `environment:`/`build.args:` | Validation error stating where interpolation is allowed |
| Non-build variable in `build.args` | Error explaining image history exposure and the build flag |
| Archive too large / zip bomb / unsafe path | 413 or 400 with the reason; nothing enqueued, nothing left on disk |
| Unsupported archive format | 400 "upload a .zip or .tar.gz" |
| Upload identical to active release | 200 with existing job; `--force` to redeploy |
| Retained archive missing | Job fails: "upload archive unavailable; upload it again" |
| Deploy changes without active release | No button; API returns 409 "nothing is deployed yet" |
| Git fields on an upload project (or vice versa) | Field-level validation error |

## Testing

All behavior test-first.

- **Unit:**
  - substitution: allowed and rejected contexts, `:-` defaults, `$$`, missing
    names (all listed), build flag enforcement, list and mapping
    `environment:` forms;
  - `.env` parser: comments, `export`, quoting, escapes, invalid names;
  - digest canonicalization and pending state, including legacy releases;
  - extraction: traversal, absolute paths, escaping and internal symlinks,
    hardlinks, special files, size and entry limits, single-root flattening,
    `__MACOSX` filtering, both formats;
  - deterministic normalization (same content in zip and tar.gz yields the
    same hash);
  - `repodeploy up` file selection with `.gitignore`, `.repodeployignore`,
    negation and `.env` exclusion.
- **Engine:** upload deduplication and `--force`; restart during
  preparing/building of an upload; deploy-changes for Git and upload projects;
  rollback restores previous variable values; archive retention and deletion;
  project deletion removes variables and archives.
- **Web/API:** CSRF and bearer enforcement on all new routes; values absent
  from pages unless revealed, from job snapshots and from logs; upload size
  limit and concurrency cap; Git/upload field validation.
- **Docker integration:** example app printing a variable, deployed via upload
  and via Git; change variable → Deploy changes → new value served; rollback →
  old value served; upload of a broken build leaves the active release serving.
- **Required checks:** `go test ./...`, `go test -race ./...`, `go vet ./...`,
  `gofmt -l cmd internal`, `go mod verify`, static Linux amd64/arm64 builds, and
  the opt-in Docker/SSH/daemon integrations when available. No new
  dependencies (stdlib `archive/zip`, `archive/tar`, `compress/gzip`).

## Documentation

README (environment variables and manual upload sections, `repodeploy up`,
`repodeploy env`), `docs/spec.md` (new section summarizing this design),
`docs/operations.md` (`--max-upload`, `uploads/` directory, backup contents).
