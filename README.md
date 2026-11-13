# herdrcron

Schedules recurring coding-agent runs (Claude Code by default) in [herdr](https://herdr.dev) panes, on this machine or on remote machines over SSH. A daemon fires the schedules and tracks each run; a TUI shows and edits tasks. Closing the TUI stops nothing.

The full spec is `herdrcron.md`. Every herdr command herdrcron uses is documented in `docs/herdr-findings.md` (herdr **0.9.1**). `docs/acceptance.md` lists how each acceptance criterion is verified.

## Install

Release archives (a tagged release builds them with GoReleaser) hold one static binary for linux/amd64, linux/arm64 or darwin/arm64:

```sh
tar xzf herdrcron_<version>_linux_amd64.tar.gz herdrcron
install -m 0755 herdrcron ~/.local/bin/
```

Or build from source with Go 1.24.2 or newer (no cgo needed; SQLite is pure Go):

```sh
CGO_ENABLED=0 go install github.com/WillemDeGroef/herdrcron/cmd/herdrcron@latest
```

On the always-on host you also need herdr 0.9.1 with its Claude integration (`herdr integration install claude`) and the agent CLI logged in.

Run the daemon as a user service:

```sh
herdrcron install-service
```

On Linux this writes and enables a systemd user unit (`Restart=on-failure`, `KillMode=process`, so stopping the daemon does not kill a `herdr server` it started in preflight) and prints a `loginctl enable-linger` hint so the service keeps running after you log out. On macOS it writes a launchd agent (`~/Library/LaunchAgents/io.github.willemdegroef.herdrcron.plist`, `KeepAlive`). Install the binary at its final path first: the unit points at the binary that ran `install-service`.

Then:

```sh
herdrcron              # the TUI
herdrcron list         # tasks with state, last run and next fire
herdrcron run <id>     # run a task now
herdrcron check <target>
herdrcron logs -f [--task <id>]
herdrcron help         # every command
```

## Configuration

Config lives in `$XDG_CONFIG_HOME/herdrcron/` (default `~/.config/herdrcron/`); task and prompt files are plain files you can edit by hand and keep in git. The daemon picks up edits within 2 s.

```
config.yaml            # global settings and targets
tasks/<task-id>.yaml   # one file per task
prompts/<task-id>.md   # the task's prompt
```

State lives in `$XDG_STATE_HOME/herdrcron/` (default `~/.local/state/herdrcron/`): `herdrcron.db` (SQLite run history), `daemon.log` (JSON, rotated at 10 MB, 5 files) and `daemon.sock`.

`config.yaml` (every key is optional; the values shown are the defaults unless marked):

```yaml
timezone: Europe/Brussels   # default: the host's local zone
poll_interval: 15s          # how often active runs are polled
history_days: 90            # run history kept
catch_up_window: 24h        # how old a missed fire may be and still run (missed: run_once)
herdr_version: "0.9.1"      # expected on every target; a mismatch is a warning
attach_mode: pane           # pane | full
notify:                     # example; no notifications without a command
  command: ["ntfy", "publish", "herdrcron"]   # argv; the message is appended as one more argument
  on: [blocked, failed, timeout, lost, target_unreachable]   # omitted: every event
targets:                    # example; "local" always exists and needs no entry
  vps:
    host: vps               # SSH alias or user@host; default: the target name
    shell: bash -lc         # default
    max_parallel: 4
  buildbox:
    host: willem@10.0.0.12
    herdr_path: /usr/local/bin/herdr   # default: herdr on PATH
```

The daemon reads `config.yaml` when it starts and on the `reload` API call, which applies changes without a restart; only a target with active runs keeps its old `host`, `shell` and `herdr_path` until a `reload` after those runs have ended.

A task (`tasks/nightly-deps.yaml`; create it in the TUI with `n`, or by hand):

```yaml
id: nightly-deps              # equals the file name
target: local                 # "local" or a target from config.yaml
cwd: ~/src/myrepo             # path on the target
schedule: "0 7 * * 1"         # 5-field cron, or @every 2h, @daily, @hourly
agent:
  command: claude
  args: ["--permission-mode", "acceptEdits"]
prompt_file: prompts/nightly-deps.md   # default: prompts/<id>.md
timeout: 2h                   # the run is stopped after this
overlap: skip                 # skip | queue | parallel
missed: run_once              # skip | run_once
on_done: close_tab            # close_tab (default) | keep_tab
on_blocked: notify            # notify | stop
```

Each run opens a tab labelled `hc-<task>-<HHMM>` in a workspace labelled `hc-<repo>` in your default herdr session, launches `<command> <args…> "$(cat <prompt file>)"` there, and follows the pane's agent status until the run is done, blocked, failed, timed out or stopped. With `on_done: close_tab` (the default) the tab closes when the run ends; the last 200 lines of output stay in the run history.

### Prompt size

Prompts are limited to **131071 bytes**, on every platform. Linux limits one command-line argument to 128 KiB including its terminating NUL, and the shell in the pane passes the prompt to the agent as one argument. herdrcron refuses a bigger prompt when it validates the task: on load the task shows as `invalid`, and the TUI form and `tasks.create`/`tasks.update` give a field error on the prompt. It does not fall back to typing the prompt into the agent with `herdr pane send-text`: that path has the same per-argument limit, needs a second launch path that waits for the agent's input box, and pasting 128 KiB into a TUI is fragile. Keep long instructions in a file in the repo and point to it from a short prompt. See `docs/herdr-findings.md`, "Prompts above 128 KiB".

## Remote machines

A target other than `local` is reached with `ssh` from the always-on host (connection sharing through a control socket in the state dir). Each remote machine needs:

- herdr **0.9.1** (or the `herdr_version` you set) on the PATH of the target's `shell` (default `bash -lc`), or at `herdr_path`;
- herdr's Claude integration: `herdr integration install claude`;
- the agent CLI installed and logged in (for example `claude` after `claude login`);
- the repos cloned at the tasks' `cwd`;
- SSH key access from the always-on host without a password prompt (an alias in `~/.ssh/config` works as `host`).

herdrcron starts the remote `herdr server` itself when none runs. `herdrcron check <target>` (or `t` in the TUI) checks reachability, the herdr server, its version, and the agent command and cwd of every task on that target. When a target does not answer, fires are recorded as `host_unreachable` and polls back off up to 5 minutes; with `missed: run_once` a missed fire runs once when the target is back, within `catch_up_window`.

`a` in the TUI attaches to a run: on a remote target with `attach_mode: pane` it runs `ssh -t <host> … herdr agent attach <pane>`; with `attach_mode: full` it opens the remote herdr with `herdr --remote <host>`.

## Develop

Go is the only dependency.

```sh
go build ./... && go vet ./... && go test -race ./...
go run ./cmd/herdrcron daemon   # foreground; logs JSON to $XDG_STATE_HOME/herdrcron/daemon.log
go run ./cmd/herdrcron list     # asks the running daemon
```

Integration tests (build tag `integration`) run a real herdr on localhost in an isolated throwaway session (`herdr --session hc-it-…`, under a temporary HOME for the daemon test); they never touch your own herdr server, and they skip when herdr is not on the PATH:

```sh
go test -tags integration -run RealHerdr ./cmd/herdrcron/ ./internal/runner/
HERDRCRON_SSH_TEST_HOST=<ssh alias> go test -tags integration -run SSHIntegration ./internal/target/
```

Releases: pushing a tag `v*` runs `.github/workflows/release.yml`, which builds the archives with GoReleaser (`.goreleaser.yaml`). Check the config locally with `goreleaser check && goreleaser release --snapshot --clean` (output in `dist/`).
