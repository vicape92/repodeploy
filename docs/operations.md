# Configuration and recovery boundaries

## Compose subset

One YAML document. Top-level fields: `services`, `volumes`, `networks`, `secrets`,
`configs`. No includes, extends, env files, profiles, remote build
contexts, privileged mode, fixed container names, host networking, arbitrary bind
mounts, anonymous volumes, additional capabilities or socket mounts.
Image-declared `VOLUME` destinations also require explicit named mounts; otherwise
the deployment is rejected before activation. A literal dollar sign is written
`$$`, as Compose requires (for example `test: ["CMD-SHELL", "test -n \"$$HOME\""]`);
project variables are interpolated only as `${NAME}` or `${NAME:-default}` inside
`environment` values and `build.args` (inserted values keep `$` literal); any
other `$` is rejected as interpolation.

Services that use `image:` reuse a tag already present on the host, so moving
tags such as `:latest` stay frozen. Select **Image updates → Pull newer images on
every deployment** (`"pull_images": true`) to pull them, and Dockerfile base
images, on each deployment. Released manifests always pin image IDs, so rollback
is unaffected.

Service fields: `build`, `image`, `command`, `entrypoint`, `environment`, `ports`,
`expose`, `healthcheck`, `depends_on`, `restart`, `volumes`, `networks`, `secrets`,
`configs`, `working_dir`, `user`, `init`, `stop_grace_period`, `read_only`, `tmpfs`,
`cap_drop`. Environment and build arguments must have explicit values; host
environment inheritance is disabled. Build supports `context`, `dockerfile`,
`args`, `target`. Context and Dockerfile must resolve within the checkout;
symlinks cannot escape it. Build paths are relative to repository root.

The engine adds to every service `security_opt: [no-new-privileges:true]`, which
stops setuid binaries such as `sudo` from gaining privileges inside containers,
and a `json-file` logging driver capped at 3 × 10 MB. Repositories cannot set
`security_opt` or `logging` themselves. If an image genuinely needs setuid
escalation, set **Privilege escalation → Allowed** in the project settings
(`"allow_privilege_escalation": true` in JSON).

### Published ports

Dockerfile mode publishes the host port on `127.0.0.1` by default, so the
application is reachable only through a reverse proxy on the same host. Choose
`0.0.0.0` (or `::`) under **Publish port on** (`"bind_address"` in JSON) to expose
it on every interface. Docker inserts its own firewall rules, so a port published
on all interfaces bypasses UFW/firewalld.

In Compose mode, write the host IP explicitly, for example
`"127.0.0.1:18080:8080"`. Mappings without a host IP (`"18080:8080"`, a bare
port, or long syntax without `host_ip`) still work but are published on all
interfaces; the deployment log warns about each one. An explicit `0.0.0.0` is
treated as deliberate and not reported.

Named volumes use `data:/data[:ro]` syntax and an empty top-level definition:

```yaml
volumes:
  data: {}
```

The engine assigns a stable host volume name for the project. Service names and
volume definitions must stay stable across retained releases. Changing topology
needs a separate manual migration outside the MVP. Never store persistent data
only in a container's writable layer.

Managed secrets and config files use `external: true` in repository configuration:

```yaml
services:
  app:
    image: your-image:tag
    secrets: [app_password]
secrets:
  app_password:
    external: true
```

Install the value outside Git from the project page (**Secrets and configs**:
paste or upload; the page lists stored names, those the active release requires
and any that are missing, but never displays values), or with:

```sh
sudo -u repodeploy repodeploy secret PROJECT_ID app_password --file /secure/path/password
```

Pasted values have browser line endings normalised to `\n`; uploaded files are
stored byte for byte. The API also offers `GET /api/projects/ID/secrets` (names
and metadata only) and `DELETE /api/projects/ID/secrets/NAME`.

The engine snapshots these files into each release and mounts them through
Compose. Restoring a release restores its secret/config snapshot, which may be
inappropriate after a credential rotation; consider that before rolling back.
Secrets are local files protected by filesystem permissions, not encrypted
against the host administrator. Known values are filtered before deployment logs
are persisted: each stored value of 4 or more characters, and each line or
`KEY=value` value of 8 or more characters, is replaced by `[REDACTED]`. Shorter
fragments such as `true` or `8080` are left alone so ordinary log text stays
readable; do not rely on masking for very short secrets. A repository can deliberately encode or disclose any secret it
receives; filtering does not prevent that.

## Failure behavior

| Condition | Behavior / recovery |
| --- | --- |
| Git/network/credentials fail | Deployment fails before activation; active version stays running. The job error and log include Git's own diagnostic (credentials redacted). Fix access and deploy manually. |
| Branch advances during build | Persisted SHA remains the target; a later detection can queue another commit. |
| Build or pull fails | Existing containers are untouched. |
| Startup or health fails | Restore prior images/manifests and check their health. Failure stays recorded even if recovery succeeds. |
| First deployment fails activation | Remove this project's containers/network, preserving volumes; no prior release exists. |
| Recovery also fails | Clear the healthy-active claim, pause/block the project. Restore a retained healthy release manually or inspect the host. |
| Engine stops in preparation/build | Restart from a fresh checkout using the persisted SHA. |
| Engine stops in activation/checking | Conservatively restore the recorded previous release before new work. |
| Manual rollback succeeds | Use retained images without Git/build; automation stays paused until resumed. |
| Missing Docker images/artifacts | Recovery can fail. Preserve Docker storage and back up required images separately. |
| Disk full / unavailable Docker | Visible failure or service error; restore capacity/access before retrying. |

Health is a deployment-time gate, not continuous monitoring. An application that
becomes unhealthy after deployment is not automatically rolled back. All services
must be running; configured Docker healthchecks must be healthy. Alternatively,
an HTTP health URL must return a 2xx response without redirects. These checks do
not prove application-level correctness or database compatibility.

## Detection limits

Polling checks the latest branch commit at the configured interval (default 60s,
minimum 10s). It can miss intermediate commits and observes force pushes as branch
state changes. Failures back off up to 15 minutes. It works behind NAT with no
inbound port. The schedule resets after a daemon restart; commit deduplication is
persistent. An unchanged automatic target is not retried after failure; manual
deploy can retry it. If the branch moves away and returns to an older commit,
that is a new observed change and can deploy again.

GitHub requires public HTTPS access to the webhook endpoint. Only signed push
events for the configured repository and branch are accepted. Duplicate delivery
IDs and automatic commit requests are deduplicated. A `202` means the request was
durably accepted or intentionally ignored, not that deployment succeeded.
Before building any automatic deployment, the worker checks the current branch head.
A superseded target is recorded as failed without activation and the latest target
is reconciled, preventing late deliveries from downgrading the application.
Outages may require GitHub redelivery or an explicit deploy. Select `both` for
polling reconciliation of missed deliveries. Deleting the deployment branch does
not stop an existing application. Branch protection and repository write access
remain the Git provider's responsibility.

## State, retention and permissions

The state directory contains `state.db`, SQLite WAL files, `engine.lock`, protected
administrator credentials, `secrets/` (including `<project>.env.json` variable
files), `releases/JOB_ID/`, `logs/JOB_ID.log` and `uploads/<project>/<sha256>.tar.gz`
(normalised upload archives; `uploads/tmp/` holds in-flight files, swept after
24 hours). An upload archive is kept while any retained release references it, and a
freshly stored archive is kept for at least one hour before retention can
delete it.
`serve --max-upload` (default 200MB) caps archives; extraction is capped at five
times that size and 50 000 entries. Backups include `uploads/` except `tmp/`.
Never put this directory inside a Git checkout. Do not share it between hosts or
put SQLite on a network filesystem.

Retention defaults to five visible releases, configurable from 1 to 50. Active
and recovery-required releases remain protected even when that exceeds the
configured limit. Log files are capped at 1 MiB per deployment. Small deduplication
tombstones outlive visible release history so old redeliveries do not redeploy.
Only project-owned release tags and artifacts are eligible for cleanup. No
application volume is deleted by deployment, rollback or retention.

## Deleting a project

**Delete project** on the project page (type the name to confirm), or
`repodeploy delete PROJECT_ID --yes`, stops the application (`compose down`),
removes containers and networks labelled for the project, its built images,
release directories, logs, stored secrets, webhook secret and history.
Named volumes are kept unless you tick **Also delete the named volumes** or pass
`--volumes`; only volumes named `pv-PROJECT_ID-*` are touched. Deletion is refused
while any deployment step is running. Provider connections and generated SSH keys
are kept; an SSH key no longer used by any project is removed by the daily sweep.
Docker's shared BuildKit cache and externally sourced image tags are not globally
pruned; maintain those separately using the host's own storage policy.

The single global worker serializes all projects; a long build delays others.
This is deliberate for a small host. There is no exactly-once transaction spanning
SQLite and Docker: the journal and conservative restart recovery handle the
uncertain window. Run the daemon through systemd so stopping it also stops its
subprocesses. Normal rollback can interrupt traffic and cannot undo side effects
from a partially activated multi-service application.
The internal deployment timeout defaults to 30 minutes. Exhausting it fails the
job and lets the queue advance; a service shutdown instead leaves pre-activation
work recoverable on restart.
