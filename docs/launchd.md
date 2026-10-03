# Running CLIProxyAPI with launchd on macOS

Use a compiled binary and a per-user LaunchAgent to run the proxy independently
of a terminal. launchd starts it at login and restarts it after an exit, including
a clean exit. Restart attempts are throttled to at most one every ten seconds.
This does not detect a hung process, keep a sleeping Mac awake, or preserve
in-flight requests and WebSocket sessions after a crash.

This repository supports preparing the service with `scripts/launchd/prepare.sh`.
The script only writes a plist. Loading, stopping, and restarting remain explicit
`launchctl` operations. No sudo, package manager, or shell wrapper is required.
See Apple's [launchd guide](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)
and the installed `man launchd.plist` and `man launchctl` references.

## Prepare a deployment

Build from the intended checkout and retain the previous binary for rollback.
Do not use `go run` as the supervised command. Use absolute paths and a stable
working directory, since `.env`, application logs, and relative state paths
depend on it. launchd does not inherit your interactive shell's environment or
expand `~` inside plist values. The proxy itself loads `.env` from its working
directory. Keep credentials there or in the configured OAuth directory, never
in plist arguments or committed files.

Example from the repository root:

```sh
(
set -eu
deployment_root="$PWD"
revision=$(git rev-parse --short HEAD)
deployment_directory="$HOME/Library/Application Support/CLIProxyAPI/deployments"
umask 077
mkdir -p "$deployment_directory"
staging=$(mktemp -d "$deployment_directory/$revision.XXXXXX")
go build -ldflags "-X main.Commit=$revision" \
  -o "$staging/cli-proxy-api" ./cmd/server
sh scripts/launchd/prepare.sh \
  com.cliproxyapi.server \
  "$staging/cli-proxy-api" \
  "$deployment_root/config.yaml" \
  "$deployment_root" \
  "$HOME/Library/Logs/CLIProxyAPI/launchd" \
  "$staging/com.cliproxyapi.server.plist"
)
```

Record the printed plist path. Keep the staging directory: it contains the
versioned deployment binary. This unique directory avoids overwriting an old
build even when rebuilding the same revision. For a release, use a clean tree
and record `git status --short` alongside the revision so local changes are not
mistaken for a reproducible commit build. The preparer refuses to overwrite an
existing plist. Config remains external and is not copied or changed.
Keep deployment binaries outside the checkout so repository cleanup cannot
remove the executable needed for automatic restart.

Preparation deliberately stays outside `~/Library/LaunchAgents`, where a plist
could start automatically at the next login before handoff is approved.

Enable application log rotation in the deployment config:

```yaml
observability:
  logs:
    debug: false
    logging-to-file: true
    logs-max-total-size-mb: 256
    error-logs-max-files: 10
    request-log: false
```

Merge these fields into the existing section. Do not replace other observability
settings. Application logs normally live under `WORKING_DIRECTORY/logs`; a
configured writable storage path can relocate them; if the working directory's
logs are not writable, the application falls back to the OAuth directory's logs.
launchd captures stdout and
stderr separately, including Go panic stacks (`GOTRACEBACK=all`). These two files
are **not rotated by launchd or the application's log cleaner**. Check their size
and archive them while the service is stopped during maintenance. Enable a
separate rotation policy if they grow substantially. Keep log directories
private; the preparer uses mode 0700 for new directories and 0600 for its plist,
but does not change permissions on existing directories.

## Production handoff

Do not start the service while another proxy owns the same port. Identify the
listener first, and get explicit production approval before stopping it:

```sh
lsof -nP -iTCP:8317 -sTCP:LISTEN
```

After the existing owner has been stopped in the approved maintenance window:

```sh
(
set -eu
# Replace this with the actual path printed during preparation.
staged_plist=/absolute/path/to/prepared/com.cliproxyapi.server.plist
mkdir -p "$HOME/Library/LaunchAgents"
# Refuse to replace an existing installation; use the upgrade procedure below.
ln "$staged_plist" "$HOME/Library/LaunchAgents/com.cliproxyapi.server.plist"
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.cliproxyapi.server.plist"
launchctl print "gui/$(id -u)/com.cliproxyapi.server"
lsof -nP -iTCP:8317 -sTCP:LISTEN
)
```

If a previous explicit `launchctl disable` persists, first run
`launchctl enable "gui/$(id -u)/com.cliproxyapi.server"`. A loaded job may be
waiting to restart; inspect its state and last exit code before assuming failure.
Inspect local logs carefully without printing credentials or complete request
bodies into agent transcripts.

For intentional shutdown, use `bootout`, not `kill`: KeepAlive restarts a killed
process. `bootout` unloads the job for the current login session; leaving its
plist under `~/Library/LaunchAgents` allows it to start at the next login.

```sh
launchctl bootout "gui/$(id -u)/com.cliproxyapi.server"
```

For permanent disablement, also run
`launchctl disable "gui/$(id -u)/com.cliproxyapi.server"` or archive the plist
outside `~/Library/LaunchAgents`. Never overwrite a binary while it is running.

For an approved upgrade, prepare a fresh deployment first, then run:

```sh
(
set -eu
staged_plist=/absolute/path/to/new/com.cliproxyapi.server.plist
installed="$HOME/Library/LaunchAgents/com.cliproxyapi.server.plist"
backup=$(mktemp -d "$HOME/Library/LaunchAgents/../cliproxy-rollback.XXXXXX")
test -f "$staged_plist"
test -f "$installed"
# Preflight the replacement before stopping anything; publish on the same volume.
cp "$staged_plist" "$backup/new.plist"
chmod 600 "$backup/new.plist"
/usr/bin/plutil -lint "$backup/new.plist"
cp "$installed" "$backup/com.cliproxyapi.server.plist"
launchctl bootout "gui/$(id -u)/com.cliproxyapi.server"
# Teardown is asynchronous. Wait for this exact job to disappear before proceeding.
attempt=0
while launchctl print "gui/$(id -u)/com.cliproxyapi.server" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  test "$attempt" -lt 30 || exit 1
  sleep 1
done
mv "$backup/new.plist" "$installed"
if ! launchctl bootstrap "gui/$(id -u)" "$installed"; then
  printf 'Activation failed. Retained previous plist: %s\n' "$backup/com.cliproxyapi.server.plist" >&2
  exit 1
fi
launchctl bootstrap "gui/$(id -u)" "$installed"
printf 'Previous plist retained at %s\n' "$backup/com.cliproxyapi.server.plist"
)
```

Retain both deployment directories and verify the replacement. Run upgrade
steps in an attended window: a failure after bootout needs immediate rollback.
If bootstrap fails, inspect whether the job loaded before deciding whether
bootout is needed. To roll back, bootout the replacement if loaded, wait for
teardown as above, then restore the retained previous plist:

```sh
(
set -eu
previous=/absolute/path/to/cliproxy-rollback.XXXXXX/com.cliproxyapi.server.plist
installed="$HOME/Library/LaunchAgents/com.cliproxyapi.server.plist"
archive=$(mktemp -d "$HOME/Library/cliproxy-failed-deployment.XXXXXX")
test -f "$previous"
if test -f "$installed"; then cp "$installed" "$archive/failed.plist"; fi
cp "$previous" "$archive/restored.plist"
mv "$archive/restored.plist" "$installed"
launchctl bootstrap "gui/$(id -u)" "$installed"
)
```

The previous plist still points to the previous binary. Config changes need
their own retained copy and deliberate rollback; restoring a plist alone does
not restore configuration.

A LaunchAgent needs a logged-in user and stops at logout. For service before
login, a LaunchDaemon requires a separate administrator-approved installation,
explicit user ownership and environment setup. For continuous remote access,
use an always-on host and its native service manager. Tailscale Serve forwards
traffic; it does not supervise this process or prevent laptop sleep.

## Port ownership and agent testing

Local deployment conventions:

| Purpose | Address | Ownership |
| --- | --- | --- |
| Production API | `127.0.0.1:8317` | Production owner only; agent sessions depend on it |
| Isolated test API | `127.0.0.1:18317` | Check availability first; reserve another free high port for parallel tests |
| Optional production profiling | `127.0.0.1:8316` | Only if explicitly enabled; disable or relocate for tests |

These are local conventions, not new server defaults. Set the test port in
`server.port` and loopback binding in `server.host` of a **separate config**.
There is no server `--port` flag. Use `--config /absolute/test/config.yaml`.
The OAuth `--oauth-callback-port` flag changes only the login callback listener.
Do not change production Tailscale forwarding or client defaults for a test.

Agents must use a fresh private working directory and config. Do not blindly
copy production configuration: it can enable exporters, shared storage, Home
integration, discovery, plugins, or other writable state. Use a minimal config,
disable profiling/discovery/Trace Flow unless specifically under test, and use
dedicated credentials and state directories. A different API port alone does
not isolate credential refreshes, `.env`, logs, databases, or telemetry outboxes.
Never share production outbox or credential files with a test writer. If testing
an exporter, use a dedicated test destination, key, and private outbox.

For launchd tests, use a distinct label such as `com.cliproxyapi.test` and a
staged plist outside `~/Library/LaunchAgents`, so it cannot start next login.
Bootstrap it explicitly and bootout that exact label during cleanup. Track the
test PID; never use `pkill`, broad process matching, or a production PID file.
Do not modify `8317` or its owner while an agent is using the proxy.

## Done: verification and crash diagnosis

Run the repeatable supervision test on macOS with Python 3 and a logged-in GUI
session. Supply a compiled test binary, not the production process:

```sh
python3 scripts/launchd/test.py /absolute/path/to/test-binary
```

The harness chooses a free local port and a unique job label. It validates
escaped paths, private permissions, overwrite refusal, authenticated HTTP
service, automatic restart after a deliberate abort, and captured crash output.
It unloads the test job and retains private temporary artifacts for diagnosis.
It has no upstream credentials, so it does not replace the model/tool check.

- Confirm the job's PID owns the intended port and the config stays loopback-only.
- Run an authenticated model request and tool round trip against the intended
  instance. A listener, model list, or WebSocket HTTP 101 alone is insufficient.
  Confirm the expected upstream connection and response, using dedicated test
  credentials for isolation tests.
- In the isolated launchd instance, terminate its exact PID and confirm launchd
  assigns a new PID, restores HTTP service, and retains diagnostic output.
  Test production recovery only during an approved maintenance window.
- Retain the deployed revision, plist, config location, and previous binary for
  rollback. Confirm login startup at the next planned login without interrupting
  active clients.

On the next exit, record the timestamp, launchctl's last exit status, stderr
panic output, application logs, and any matching macOS DiagnosticReport. No
matching panic does not prove a clean shutdown: signals, resource termination,
or a lost terminal can leave different evidence. Do not automatically restart
for upstream quota errors; launchd handles process exits, not provider health.
