# herdrcron — Spec

Nov 6, 2026 · @Willem

herdrcron is a single Go binary that schedules recurring coding-agent runs (Claude Code with a predefined prompt) and runs them inside herdr panes on the local machine and on remote machines over SSH. A daemon on one always-on machine owns all schedules; a TUI shows and edits them.

## Overview and goals

The user has one machine that runs 24/7. That machine runs the herdrcron daemon. The daemon starts agents on that machine and on remote machines. Every agent runs in a herdr pane, so the user can attach and watch or take over at any time.

Goals:

- Define recurring tasks: schedule, target machine, repo, agent command, prompt.
- Run each task on time inside a new herdr tab on the target machine.
- Show the current state of all tasks on all machines in one TUI.
- From the TUI: enable or disable, pause or resume, add, edit, delete, run now, and attach to a running task.
- Keep schedules running when the TUI is closed.
- Survive a daemon restart without losing track of running tasks and without starting duplicate runs.

Non-goals (v1):

- No web UI. No multi-user access control.
- No herdrcron component on remote machines. Remote machines need only herdr, the agent CLI and SSH access.
- No support for the new herdr app (`hc`, herdr-core). Target the original herdr TUI and its `herdr` CLI only.
- No pause of an agent in the middle of a run. A running agent can only be stopped.
- No task dependencies or chains between tasks.

## Architecture

One binary, two modes. The daemon is the only component that has state and that runs schedules. The TUI is a thin client.

| Component | Runs on | Responsibility |
| --- | --- | --- |
| `herdrcron daemon` | Always-on host, as a systemd user service (Linux) or launchd agent (macOS) | Loads tasks, runs the scheduler, starts and tracks runs, stores history, serves the API |
| `herdrcron` (TUI) | Always-on host; from other devices via `ssh -t host herdrcron` | Shows state, edits tasks, sends commands to the daemon |
| Target `local` | Always-on host | Runs `herdr …` commands directly |
| Target `<ssh-alias>` | Any remote machine | Runs `herdr …` commands over SSH, using the user's `~/.ssh/config` |
| herdr server | Each target | Owns panes and agent processes; reports agent state |

Rules:

- The daemon talks to herdr only through the `herdr` CLI with JSON output. Do not implement the raw socket protocol in v1. The socket is local to each machine, and the CLI is the more stable contract.
- The TUI talks to the daemon only through the daemon API (see Daemon API). The TUI never reads or writes task files directly.
- All herdr workspaces, tabs and panes that herdrcron creates carry a `hc-` label prefix, so herdrcron can find them again and the user can tell them apart. (For herdr 0.9.1, `docs/herdr-findings.md` labels workspaces and tabs only. Panes are found by the stored pane id and by `tab_id`: `tab create --label` does not label the pane.)
- Language: Go 1.23+. Suggested libraries: Bubble Tea, Bubbles and Lip Gloss (TUI), Huh (forms), robfig/cron v3 (cron parsing), golang.org/x/crypto/ssh or the system `ssh` binary (see Run execution), modernc.org/sqlite (pure Go, no cgo).

## Data model

Task definitions are plain files that the user can also edit by hand and keep in git. Run history and runtime state live in SQLite.

Layout under `$XDG_CONFIG_HOME/herdrcron/` (default `~/.config/herdrcron/`):

```
config.yaml            # global settings and target list
tasks/<task-id>.yaml   # one file per task
prompts/<task-id>.md   # prompt text for the task
```

State under `$XDG_STATE_HOME/herdrcron/` (default `~/.local/state/herdrcron/`): `herdrcron.db` (SQLite), `daemon.log`, `daemon.sock`.

### Task file

```yaml
id: peppol-sh-deps            # slug, unique, equals the file name
name: peppol.sh dependency update
target: vps                   # "local" or a target name from config.yaml
cwd: ~/src/peppol-sh          # path on the target
schedule: "0 7 * * 1"         # 5-field cron, or @every 2h, @daily, @hourly
timezone: Europe/Brussels     # optional; default from config.yaml
enabled: true
paused_until: null            # null, RFC 3339 time, or "indefinite"
agent:
  command: claude             # executable on the target
  args: ["--permission-mode", "acceptEdits"]
prompt_file: prompts/peppol-sh-deps.md
timeout: 2h                   # Go duration; run is stopped after this
overlap: skip                 # skip | queue | parallel
missed: run_once              # skip | run_once
on_done: close_tab            # close_tab (default) | keep_tab
on_blocked: notify            # notify | stop
env:                          # optional extra env vars for the agent
  FOO: bar
```

Validation on load and on every write: `id` matches `^[a-z0-9][a-z0-9-]{0,62}$`; target exists; schedule parses; prompt file exists; timeout > 0. An invalid file does not stop the daemon. The task shows state `invalid` with the error text in the TUI.

### Run record (SQLite table `runs`)

| Column | Type | Meaning |
| --- | --- | --- |
| `id` | TEXT (ULID) | Run id |
| `task_id` | TEXT | Task id |
| `target` | TEXT | Target name at start time |
| `trigger` | TEXT | `schedule`, `manual`, `catch_up` |
| `scheduled_at` | TIMESTAMP | Planned fire time |
| `started_at` / `ended_at` | TIMESTAMP | Actual times; `ended_at` null while active |
| `status` | TEXT | See run states below |
| `herdr_workspace` / `herdr_tab` / `herdr_pane` | TEXT | herdr ids, for reattach and recovery |
| `exit_reason` | TEXT | Short human text, for example `timeout after 2h` |
| `output_tail` | TEXT | Last 200 lines of pane output at the end of the run |

Run states: `starting`, `running`, `blocked`, `done`, `failed`, `timeout`, `stopped`, `lost` (pane no longer exists), `host_unreachable`.

Table `task_state` holds per task: `last_run_id`, `next_fire_at`, `last_fire_at`. Keep runs for 90 days by default (`history_days` in config.yaml).

## Scheduler semantics

A task fires only when it is enabled, not paused, valid, and its fire time is reached. "Off" is a permanent switch; "paused" is temporary and keeps the schedule.

| Display state | Condition | Scheduler action |
| --- | --- | --- |
| `invalid` | Task file fails validation | Never fires |
| `off` | `enabled: false` | Never fires; `next_fire_at` is empty |
| `paused` | `paused_until` is `indefinite` or in the future | Fire times pass without a run; no catch-up for them |
| `running` | Active run in `starting` or `running` | Next fire follows `overlap` |
| `blocked` | Active run waits for the user (herdr agent status `blocked`) | Next fire follows `overlap`; notify once |
| `idle` | None of the above | Fires at `next_fire_at` |

Rules:

1. Compute `next_fire_at` from the cron expression in the task timezone. Recompute after each fire, each edit, each enable, and each resume.
2. Pause sets `paused_until`. When `paused_until` passes, the daemon clears it and writes the task file. Resume clears it at once.
3. Global pause: `pause_all` in the daemon state pauses every task on one target or on all targets, without changing task files. The TUI shows it in a banner.
4. Overlap when a fire time arrives and a run is still active:
   - `skip`: record nothing, log one line, compute the next fire.
   - `queue`: keep at most one pending fire; start it when the active run ends.
   - `parallel`: start a new run in a new tab.
5. Missed fires (daemon was down, or the target was unreachable at fire time):
   - `skip`: do nothing.
   - `run_once`: start one run with trigger `catch_up` when the daemon starts or the target is reachable again, no matter how many fires were missed. Do not catch up fires older than 24 h (`catch_up_window` in config.yaml).
6. Run now: starts a run with trigger `manual`. It ignores `enabled` and `paused`, but follows `overlap` (with `skip`, the TUI asks to confirm a parallel run).
7. Edits apply to the next run only. A running run is not changed.
8. Delete: refuse while a run is active, unless the user confirms "stop and delete". Delete removes the task file and prompt file; run history stays until it expires.

## Run execution on targets

Each run gets a new herdr tab in a per-repo workspace on the target, with the agent started interactively so the user can attach. The daemon tracks the run by polling, never by a long blocking call over SSH.

### Target adapter

Put all herdr calls behind one Go interface, so local and SSH targets share the run logic and tests can use a fake:

```go
type Target interface {
    Name() string
    Exec(ctx context.Context, argv []string, stdin io.Reader) (stdout []byte, err error)
    PutFile(ctx context.Context, remotePath string, data []byte) error
}
```

- `local`: `os/exec`, no shell.
- SSH: the system `ssh` binary, so `~/.ssh/config`, agents, keys and ProxyJump work as the user expects. Always pass `-o BatchMode=yes -o ControlMaster=auto -o ControlPath=$XDG_STATE_HOME/herdrcron/ssh/%C -o ControlPersist=10m -o ConnectTimeout=10`. Quote every argument with a POSIX shell quoter; never build a command string from unquoted user data. The remote login shell may be a non-POSIX shell (fish, tcsh): send every command as `/bin/sh -c …` in a form all login shells read the same, and let `/bin/sh` read the quoted command. A cancelled call also stops its command on the remote (see `docs/herdr-findings.md`, "SSH: login shell and cancellation").
- Remote PATH: non-interactive SSH shells often lack `herdr` and `claude` on PATH. Each target in `config.yaml` has `shell` (default `bash -lc`) and optional `herdr_path`. Run every remote command through that shell.
- SSH exit code 255 = target unreachable. Any other non-zero exit = command error.

A `herdr` package on top of `Target` wraps the herdr CLI commands and parses JSON output into Go types. Generate the types from `herdr api schema --json` of the pinned herdr version. Before coding each call, check the exact flags and JSON fields with `herdr <cmd> --help` and the schema; the commands below are the intent, not a verified API.

### Run steps

1. Reserve: insert the run row (`starting`). Create the run id.
2. Preflight (cached per target for 10 min): the herdr server answers (`herdr pane list` succeeds); the agent command is on PATH; `cwd` exists. On failure: run `failed` with the reason. (For herdr 0.9.1, `docs/herdr-findings.md` starts `herdr server` once, in a new session and without waiting, when the error is `server_not_running`, then retries `pane list`.)
3. Workspace: find a workspace labelled `hc-<repo-basename>`; if none, `herdr workspace create --cwd <cwd> --label hc-<repo-basename>`. (For herdr 0.9.1, `docs/herdr-findings.md` matches the label only—there is no reliable workspace cwd on `WorkspaceInfo`, so every run relies on `tab create --cwd` instead of a cwd match.)
4. Tab: create a tab in that workspace labelled `hc-<task-id>-<HHMM>`. Store workspace, tab and root pane ids in the run row. (For herdr 0.9.1, `docs/herdr-findings.md` reuses the first tab `workspace create` already made, via `tab rename`, and looks the workspace up by label on every run: closing the last tab closes the workspace. The run row still stores the ids for that run.)
5. Prompt: `PutFile` the prompt to `~/.cache/herdrcron/runs/<run-id>.md` on the target (for SSH: `ssh <alias> 'mkdir -p … && cat > …'` with the prompt on stdin).
6. Launch: `herdr pane run <pane> "cd <cwd> && env HERDRCRON_RUN_ID=<id> <ENV…> <command> <args…> \"$(cat <prompt-path>)\""`. The prompt goes in as the first argument, so there is no need to wait for the agent's input prompt. Set the run to `running`. (For herdr 0.9.1, `docs/herdr-findings.md` moves `cd` and `env` into `tab create --cwd/--env`.)
7. Track: poll every 15 s (`poll_interval`) with one call that returns the pane list and agent status for all active runs on that target. Map status:
   - `working` → `running`.
   - `blocked` → `blocked`; on the first change, notify (see Configuration). With `on_blocked: stop`, stop the run.
   - `done` or `idle` for 2 polls in a row (debounce) → `done`.
   - Pane gone → `lost`. (For herdr 0.9.1, `docs/herdr-findings.md` also ends a run `lost` when a restarted herdr server restored its pane id as a new shell.)
   - Unknown or no status (no herdr agent integration) → stay `running`; the timeout still applies. (For herdr 0.9.1, `docs/herdr-findings.md` ends a run whose `agent` is absent when a later `pane process-info` sample shows the shell back in the foreground.)
8. Timeout: when `started_at + timeout` passes, stop the run (step 10) with status `timeout`.
9. Finish: read the last 200 lines (`herdr pane read <pane> --source recent --lines 200`) into `output_tail`. Set `ended_at`. With `on_done: close_tab`, close the tab; else keep it for the user to read. Delete the remote prompt file. (For herdr 0.9.1, `docs/herdr-findings.md` uses `--source recent-unwrapped` so soft wraps are joined. `--source recent --lines N` does work and keeps the wraps.)
10. Stop (timeout, user stop, or `on_blocked: stop`): send Ctrl-C to the pane twice, 2 s apart; wait 10 s; then close the tab. Status `stopped` or `timeout`. (For herdr 0.9.1, `docs/herdr-findings.md` replaces this with `esc`, then `ctrl+c ctrl+c` in one call: two calls 2 s apart do not exit Claude Code. If that pane id was restored as a new shell, the run ends `lost` and no keys are sent.)

### Recovery and failures

- Daemon start: for every run with `ended_at` empty, look up its pane on the target. Pane exists → continue tracking from step 7. Pane gone → `lost`. Target unreachable → keep the run open and retry.
- Target unreachable during a run: the agent keeps running on the target. Mark the target `unreachable`, back off (30 s, doubling, max 5 min), and resume tracking when it answers. Do not count unreachable time towards a `failed` status; the timeout still counts.
- Target unreachable at fire time: record a run with status `host_unreachable`; apply `missed` when the target answers.
- Concurrency: one worker per target; at most `max_parallel` active runs per target (default 4). Fires above the limit wait in a per-target queue. Waiting fires (this queue and the one pending fire of `overlap: queue`) are stored in table `queued_fires` and wait again after a daemon restart; a fire whose task is gone or invalid by then, or a schedule or catch-up fire older than `catch_up_window`, is dropped with a log line.
- Every herdr call has a 30 s context timeout.

## Daemon API

JSON-RPC 2.0 over the Unix socket `daemon.sock` (mode 0600), newline-delimited JSON. The TUI and the CLI subcommands are the only clients.

| Method | Params | Result |
| --- | --- | --- |
| `status` | — | Daemon version, uptime, targets with reachability, global pause flags |
| `tasks.list` | — | All tasks with display state, `next_fire_at`, last run summary |
| `tasks.get` | `id` | Task definition, prompt text, last 20 runs |
| `tasks.create` | task + prompt text | Created task, or validation errors per field |
| `tasks.update` | `id`, task + prompt text, `version` | Updated task; refuse on version mismatch (file changed on disk) |
| `tasks.delete` | `id`, `stop_active` | — |
| `tasks.set_enabled` | `id`, `enabled` | Task |
| `tasks.pause` | `id`, `until` (time or `indefinite`) | Task |
| `tasks.resume` | `id` | Task |
| `tasks.run_now` | `id`, `force_parallel` | Run |
| `runs.list` | `task_id`?, `limit` | Runs, newest first |
| `runs.stop` | `run_id` | Run |
| `runs.output` | `run_id`, `lines` | Live pane output for active runs; `output_tail` for ended runs |
| `runs.attach_cmd` | `run_id` | The argv the TUI executes to attach (see TUI) |
| `targets.check` | `name` | Preflight report |
| `pause_all` / `resume_all` | `target`? | — |
| `reload` | — | Re-read config and task files |
| `subscribe` | — | Stream of events: `task.changed`, `run.changed`, `target.changed` |

Rules:

- `version` is a hash of the task file. The daemon watches `tasks/` with fsnotify; a hand edit reloads that task and emits `task.changed`.
- The daemon writes task files atomically (write temp file, then rename).
- CLI subcommands for scripts, same API: `herdrcron list`, `herdrcron run <id>`, `herdrcron pause <id> [--until 2026-11-10T08:00]`, `herdrcron resume <id>`, `herdrcron enable|disable <id>`, `herdrcron check <target>`.

## TUI

One main list of all tasks on all targets, updated live from the `subscribe` stream, with every action one key away. Every destructive action asks for confirmation.

### Main screen

```
 herdrcron · daemon up 3d · targets: local ●  vps ●  buildbox ○ unreachable
 TARGET    TASK                 SCHEDULE      STATE      LAST         NEXT
 local     peppol-sh-deps       Mon 07:00     ● idle     ✓ 3d ago     Mon 07:00
 vps       factory-coordinator  */30 * * * *  ◐ running  ✓ 28m ago    —
 vps       nightly-e2e          02:00 daily   ⏸ paused   ✗ 1d ago     until Nov 10
 vps       peppolnews-ingest    @every 2h     ○ off      ✓ 5d ago     —
 buildbox  release-notes        Fri 16:00     ! blocked  …            —
 space on/off  p pause  r run now  n new  e edit  d delete  enter details  a attach  / filter  ? help
```

- Sort: blocked first, then running, then by `next_fire_at`. Group by target with `g`.
- Colours: blocked = warning, failed or timeout = critical, running = accent, off and paused = dim.
- Header shows global pause as a banner.

### Keys

| Key | Action |
| --- | --- |
| `space` | Enable or disable the selected task |
| `p` | Pause: menu with 1 h, until tomorrow 08:00, until a date, indefinitely; `p` on a paused task resumes it |
| `P` | Pause all on the selected target, or all targets |
| `r` | Run now |
| `s` | Stop the active run (confirm) |
| `n` | New task (form) |
| `e` | Edit task (form) |
| `E` | Edit prompt in `$EDITOR` |
| `d` | Delete task (confirm; offer "stop and delete" when a run is active) |
| `enter` | Task detail |
| `a` | Attach to the active run's pane |
| `l` | Live output of the active run |
| `t` | Targets screen |
| `/` | Filter by text |
| `q` | Quit the TUI (the daemon keeps running) |

### Other screens

- Task detail: definition, next 5 fire times, last 20 runs (status, trigger, start, duration, reason). `enter` on a run shows its `output_tail`.
- Live output: polls `runs.output` every 2 s and shows the last screen of the pane, ANSI kept.
- Form (new and edit): fields from the task file. Target is a picker from `config.yaml`. Schedule shows the next 3 fire times as you type, and errors inline. Prompt opens in `$EDITOR`. Save calls `tasks.create` or `tasks.update`; field errors from the daemon show next to the field.
- Targets: name, reachability, herdr version, last preflight result; `c` runs `targets.check`.

### Attach

The TUI suspends itself (Bubble Tea `tea.ExecProcess`), runs the attach command from `runs.attach_cmd`, and comes back when the user detaches with `ctrl+b q`:

- Local: `herdr agent attach <pane>`.
- Remote: `ssh -t <alias> herdr agent attach <pane>`.
- Option in config (`attach_mode: full`): open the full herdr UI instead, with `herdr --remote <alias>` for remote targets.

## Configuration, deployment and logging

One `config.yaml` holds global defaults and the target list; a target name is the SSH alias unless `host` says otherwise.

```yaml
timezone: Europe/Brussels
poll_interval: 15s
history_days: 90
catch_up_window: 24h
herdr_version: "0.9.1"      # expected on every target; mismatch = warning in TUI
attach_mode: pane           # pane | full
notify:
  command: ["ntfy", "publish", "herdrcron"]   # argv; message text is appended
  on: [blocked, failed, timeout, lost, target_unreachable]
targets:
  vps:
    host: vps               # SSH alias from ~/.ssh/config
    shell: bash -lc
    max_parallel: 4
  buildbox:
    host: willem@10.0.0.12
    herdr_path: /usr/local/bin/herdr
```

`local` is always present and needs no entry.

Deployment:

- `herdrcron install-service` writes and enables a systemd user unit (`Restart=on-failure`, `loginctl enable-linger` hint) or a launchd agent (`KeepAlive`).
- Release builds with GoReleaser for linux/amd64, linux/arm64, darwin/arm64. Single static binary.
- Remote machines need: herdr at the pinned version, its Claude integration (`herdr integration install claude`), the agent CLI logged in, the repos cloned, and SSH key access from the always-on host.

Logging:

- `log/slog` with JSON output to `daemon.log`, rotated at 10 MB, 5 files. Every line about a run carries `run_id`, `task_id` and `target`.
- Never log prompt text or environment values; log the prompt file path and env keys only.
- `herdrcron logs [-f] [--task <id>]` prints the log, filtered.

## Testing and acceptance criteria

The scheduler and run logic must be testable without herdr or SSH: inject a fake `Target` and a fake clock.

Tests:

- Unit: cron and `@every` parsing with timezones and DST changes (Europe/Brussels, last Sunday of March and October); overlap modes; missed-run policy and `catch_up_window`; pause expiry; task validation; shell quoting with prompts that hold quotes, `$`, backticks and newlines.
- Run state machine with a fake herdr that plays scripted status sequences: working → done; working → blocked → working → done; pane vanishes; target drops for 3 polls and returns; timeout.
- Recovery: kill the daemon during a run, restart, assert one run row, tracking continues, no second run starts.
- Integration (build tag `integration`): a real herdr on localhost, task with agent command `bash -c 'echo hi; sleep 5'`; and an SSH target against a local sshd container.
- TUI: `teatest` golden-file tests for the main screen and the form.

Acceptance criteria (all must pass on the user's machines):

- [ ] A task on `local` with `*/2 * * * *` starts a Claude Code run in a new herdr tab every 2 minutes, with the prompt from its prompt file.
- [ ] The same task on a remote target starts there, and `a` in the TUI attaches to it.
- [ ] Closing the TUI does not stop any schedule or run.
- [ ] Toggle on/off, pause, resume, add, edit, delete and run now work from the TUI and change the task file on disk.
- [ ] A hand edit of a task file shows in the open TUI within 2 s.
- [ ] A run that needs a permission shows `blocked` and sends one notification.
- [ ] A run past its timeout is stopped and shows `timeout`.
- [ ] Restarting the daemon during a run does not start a duplicate and keeps tracking the run.
- [ ] With a remote target offline at fire time, the run is recorded as `host_unreachable`, and with `missed: run_once` it runs once when the target is back.
- [ ] A prompt with quotes, `$`, backticks and newlines reaches the agent unchanged.

## Milestones and open questions

Build in five steps; each step ends with working, tested software that the user can try.

1. Core: task files, validation, SQLite store, scheduler with fake clock, CLI `list` and `run`. Runs only on `local`.
2. herdr adapter and run tracking on `local`: workspace, tab, launch, poll, timeout, stop, recovery after restart.
3. Daemon API, `subscribe` stream, service install, notifications.
4. TUI: main screen, keys, form, detail, live output, attach.
5. SSH targets: adapter, preflight, reachability and backoff, catch-up, targets screen.

Open questions for the implementing agent to resolve against the installed herdr, and to report back before step 2 ends. Answered for herdr 0.9.1 in `docs/herdr-findings.md`, which supersedes the guessed commands in this spec:

- Exact CLI to create a tab inside a given workspace, to close a tab, and to find panes by label.
- Which JSON field in `herdr pane list` carries the agent status, and its values.
- How to start the herdr server headless when none runs on a target, or whether preflight must fail instead.
- Whether `herdr agent attach` accepts a pane id, or needs another target form.
- Prompt size: Linux limits one argument to 128 KiB. Above that, fall back to starting the agent bare and sending the prompt with `herdr pane send-text`, or refuse prompts above the limit. Pick one and document it.
