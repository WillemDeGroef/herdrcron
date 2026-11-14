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

### Pane shell

The launch line is typed into the pane's interactive shell, on the local machine and on a remote one alike. herdr picks that shell, not herdrcron: `default_shell` under `[terminal]` in herdr's `config.toml`, else `$SHELL`, else `/bin/sh`. A target's `shell` setting (`bash -lc`) does not change it: that one only runs herdrcron's own commands on a remote machine.

The pane shell must be POSIX-like (bash, zsh, ksh, dash). The line relies on POSIX single quoting for the command and its args (`'it'\''s'`), and on `"$(cat "$HOME"/.cache/herdrcron/runs/<run id>.md)"` to pass the prompt as one argument. csh, tcsh, nushell and PowerShell do not read that line as written, and fish treats a backslash inside single quotes differently, so an arg can reach the agent changed. herdrcron is tested with a POSIX shell only. If your login shell is another one, set `default_shell = "/bin/bash"` in herdr's config on that machine; you can still start your own shell from there in panes you open yourself.

### Prompt size

Prompts are limited to **131071 bytes**, on every platform. Linux limits one command-line argument to 128 KiB including its terminating NUL, and the shell in the pane passes the prompt to the agent as one argument. herdrcron refuses a bigger prompt when it validates the task: on load the task shows as `invalid`, and the TUI form and `tasks.create`/`tasks.update` give a field error on the prompt. It does not fall back to typing the prompt into the agent with `herdr pane send-text`: that path has the same per-argument limit, needs a second launch path that waits for the agent's input box, and pasting 128 KiB into a TUI is fragile. Keep long instructions in a file in the repo and point to it from a short prompt. See `docs/herdr-findings.md`, "Prompts above 128 KiB".

## Remote machines

A target other than `local` is reached with `ssh` from the always-on host (connection sharing through a control socket in the state dir). Each remote machine needs:

- herdr **0.9.1** (or the `herdr_version` you set) on the PATH of the target's `shell` (default `bash -lc`), or at `herdr_path`;
- herdr's Claude integration: `herdr integration install claude`;
- the agent CLI installed and logged in (for example `claude` after `claude login`);
- the repos cloned at the tasks' `cwd`;
- SSH key access from the always-on host without a password prompt (an alias in `~/.ssh/config` works as `host`);
- a POSIX-like shell in herdr panes (see "Pane shell" above);
- `/bin/sh` with `printf` and `mktemp`, and one of `setsid`, `/proc`, or a `ps` that can list pid, ppid and pgid (`ps -A -o pid= -o ppid= -o pgid=`; a BusyBox `ps` has `-o` and not `-p`, which is enough). If none of those is there, the command still runs and herdrcron says so on stderr: a cancel then stops only that command, not processes it started.

For herdrcron's own SSH commands, the remote user's login shell can be anything (bash, zsh, fish, tcsh): it only starts `/bin/sh`, which runs the command through the target's `shell`. The shell in the herdr pane is a separate matter and does have to be POSIX-like. When herdrcron gives up on a remote command (a timeout, a shutdown), the command is stopped on the remote machine too: SIGTERM first, SIGKILL five seconds later for what is still running.

### No terminal on SSH calls

The daemon's SSH calls allocate no pseudo-terminal (no `ssh -t` or `-tt`) and set `BatchMode=yes`. A terminal would change the bytes going in and out (CR LF line ends, echo), and nobody is there to answer a question. For you this means:

- `ssh` cannot ask for anything. A password, a key passphrase or an unknown host key makes the call fail, and the target shows as unreachable. Use a key that `ssh` can use unattended (an agent, or no passphrase), and connect once by hand so the host key is known.
- Nothing on the remote side can ask either. The commands herdrcron runs there (herdr, and the checks of `herdrcron check`) have no terminal: stdin is `/dev/null`, stdout and stderr are pipes. A `sudo` that wants a password, or a shell startup file that reads from the terminal, gets end-of-file. Keep the startup files of the target's `shell` (for `bash -lc`: the login profile) free of prompts.
- Closing the connection does not hang up the remote command, as it would with a terminal. herdrcron stops it itself (see above), which is why the remote needs `setsid`, `/proc` or `ps`.
- The agent is not affected: it runs in a herdr pane, which is a terminal.

Only the TUI's attach uses a terminal (`ssh -t`, below). It leaves out `BatchMode`, so there `ssh` may ask for a passphrase.

herdrcron starts the remote `herdr server` itself when none runs. `herdrcron check <target>` (or `t` in the TUI) checks reachability, the herdr server, its version, and the agent command and cwd of every task on that target. When a target does not answer, fires are recorded as `host_unreachable` and polls back off up to 5 minutes; with `missed: run_once` a missed fire runs once when the target is back, within `catch_up_window`.

`a` in the TUI attaches to a run: on a remote target with `attach_mode: pane` it runs `ssh -t <host> … herdr agent attach <pane>`; with `attach_mode: full` it opens the remote herdr with `herdr --remote <host>`.

## Upgrade and rollback

Upgrading needs no manual step: stop the daemon, replace the binary, start it. The daemon brings `herdrcron.db` to its own schema version when it opens it.

Rolling back to an older binary needs one manual step when the newer binary has raised the schema version. An older daemon refuses a newer database and exits (under systemd or launchd it is restarted and fails again):

```
herdrcron daemon: init …/herdrcron.db: database schema version 3 is newer than this herdrcron (2)
```

The message names the database's version and, in brackets, the version the old binary wants. Schema versions so far:

| Schema version | Since | Change |
| --- | --- | --- |
| 1 | first version | |
| 2 | #34 (PR #48) | column `runs.herdr_terminal` |
| 3 | #58 (PR #68) | columns `closing`, `pane_replaced`, `output_tail` on `pending_cleanups` |

To roll back:

1. Stop the daemon: `systemctl --user stop herdrcron.service`, on macOS `launchctl bootout gui/$(id -u)/io.github.willemdegroef.herdrcron`, or end a foreground `herdrcron daemon`. Do not edit the database while a daemon has it open.
2. Copy `herdrcron.db` in the state dir, with `herdrcron.db-wal` and `herdrcron.db-shm` when they exist. Do not delete the `-wal` file: it can hold committed data.
3. Set the version the old binary wants (2 here), with the `sqlite3` command-line tool. herdrcron has no command for this:

   ```sh
   sqlite3 ~/.local/state/herdrcron/herdrcron.db 'PRAGMA user_version = 2;'
   ```

4. Install the old binary and start the daemon (`herdrcron install-service` from the old binary, or `herdrcron daemon`).

Nothing else has to change. Every schema change so far only added tables and columns, and an older binary leaves what it does not know alone: the columns above, and the tables `global_pause`, `queued_fires`, `pending_cleanups` and `lost_creates`, which arrived without a new schema version. Config and task files have not changed format since #15 (PR #32). To upgrade again, just start the newer binary: it sees the columns are already there and keeps the data.

While an older binary runs, it lacks what the newer one did with that data:

- Before #60 (PR #67): a queued fire that was discarded has the status `dropped`. An older binary shows these runs as they are, but takes a dropped scheduled or catch-up fire for the task's last scheduled run, so a `host_unreachable` fire older than it is not caught up.
- Before #57 (PR #65): tabs left by a lost tab-create reply (table `lost_creates`) are not closed until a newer binary runs again.
- On schema 2, from #35 (PR #51) on: a tab close that was under way when the daemon stopped is retried from its start; the saved output tail of that run is not used.
- On schema 1: runs it starts have no `herdr_terminal`, so after you upgrade again herdrcron cannot tell for those runs that herdr replaced their pane.
- Before #35 (PR #51), which is every schema-1 binary and the first schema-2 ones: the tabs listed in `pending_cleanups` are not closed until a newer binary runs again.

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

Releases: pushing a tag `v*` runs `.github/workflows/release.yml`, which builds the archives with GoReleaser (`.goreleaser.yaml`). Check the config locally with `goreleaser check && goreleaser release --snapshot --clean` (output in `dist/`). A release that raises the schema version must say so in its notes: add a row to the table in "Upgrade and rollback"; the release notes link to that section.
