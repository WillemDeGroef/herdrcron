# herdrcron — Tickets

Source: `herdrcron.md` (spec, Nov 6, 2026). Each ticket is a vertical slice that ends in working, tested software. Tickets are numbered in dependency order: blockers come first.

## Dependency overview

| # | Ticket | Blocked by |
| --- | --- | --- |
| 01 | herdr CLI spike: answer the open questions | — |
| 02 | Daemon skeleton: task files, validation, `herdrcron list` over the API | — |
| 03 | Scheduler: next fire times, fake clock, SQLite state | 02 |
| 04 | Tracer bullet: scheduled and manual runs in a local herdr tab | 01, 03 |
| 05 | Stop and timeout | 04 |
| 06 | Overlap modes and per-target concurrency | 04 |
| 07 | Restart recovery and missed fires | 04 |
| 08 | Enable, disable, pause, resume, pause all | 03 |
| 09 | Task CRUD, hand-edit reload and the `subscribe` stream | 05, 08 |
| 10 | Notifications | 05 |
| 11 | Log rotation, `herdrcron logs` and service install | 02 |
| 12 | TUI main screen with one-key actions | 09 |
| 13 | TUI task detail, live output and attach (local) | 12 |
| 14 | TUI task form, prompt editing and delete | 12 |
| 15 | SSH targets: run and attach on a remote machine | 04, 13 |
| 16 | Target reachability, preflight, backoff and catch-up | 07, 15 |
| 17 | TUI targets screen and reachability header | 12, 16 |
| 18 | Release builds, integration suite and acceptance run | 06, 10, 11, 14, 17 |

Frontier at the start: 01 and 02.

---

# 01: herdr CLI spike: answer the open questions

**What to build:** A short findings document, checked into the repo, that answers the spec's open questions against the pinned herdr version (0.9.1; the spec said 0.7.1), plus Go types generated from `herdr api schema --json`. Every later herdr call is built on these findings, not on the spec's guessed commands.

**Blocked by:** None (can start immediately).

**Status:** ready-for-agent

- [ ] Documents the exact CLI to create a tab inside a given workspace, close a tab, and find workspaces, tabs and panes by label (`hc-` prefix).
- [ ] Documents which JSON field in `herdr pane list` carries the agent status, and all its values, and maps each to a herdrcron run state.
- [ ] Documents how to start a herdr server headless when none runs, or records the decision that preflight fails instead.
- [ ] Documents whether `herdr agent attach` takes a pane id, and the exact attach argv for local and remote.
- [ ] Records the decision for prompts above the 128 KiB argument limit (fall back to `herdr pane send-text`, or refuse) with the reason.
- [ ] Verifies `herdr pane run` and `herdr pane read --source recent --lines N` flags and output.
- [ ] Go types for the JSON outputs used are generated from the schema and compile; the generation step is repeatable.

---

# 02: Daemon skeleton: task files, validation, `herdrcron list` over the API

**What to build:** `herdrcron daemon` starts in the foreground, loads `config.yaml` and every task file, validates them, and serves JSON-RPC 2.0 on `daemon.sock`. `herdrcron list` asks the daemon and prints each task with target, schedule and display state (`invalid` with error text, `off`, or `idle`). This sets up the binary, the API transport and structured logging that every later ticket extends.

**Blocked by:** None (can start immediately).

**Status:** ready-for-agent

- [ ] Config and state directories follow `XDG_CONFIG_HOME` / `XDG_STATE_HOME` with the spec defaults; `local` target always exists.
- [ ] Task validation: id pattern, id equals file name, target exists, schedule parses, prompt file exists, timeout > 0. Unit tests cover each rule.
- [ ] An invalid task file does not stop the daemon; `herdrcron list` shows it as `invalid` with the error.
- [ ] Socket has mode 0600; API speaks newline-delimited JSON-RPC 2.0; `status` and `tasks.list` work.
- [ ] `herdrcron list` exits non-zero with a clear message when the daemon is not running.
- [ ] Logs are JSON via `log/slog` to `daemon.log`; prompt text and env values are never logged.

---

# 03: Scheduler: next fire times, fake clock, SQLite state

**What to build:** The daemon computes `next_fire_at` for every enabled, valid task in its timezone, fires at that time, and stores `task_state` and a run row in SQLite. `herdrcron list` shows NEXT and LAST. In this ticket a fire runs through a fake target, so the scheduler is fully testable without herdr.

**Blocked by:** 02

**Status:** ready-for-agent

- [ ] Supports 5-field cron, `@every <dur>`, `@daily`, `@hourly`; task timezone with fallback to the config timezone.
- [ ] Unit tests with a fake clock cover Europe/Brussels DST changes (last Sunday of March and October).
- [ ] `next_fire_at` is recomputed after each fire; `off` tasks have no `next_fire_at`.
- [ ] SQLite store (pure Go driver) with `runs` and `task_state` tables per the spec; run ids are ULIDs; trigger is recorded.
- [ ] Runs older than `history_days` are deleted.
- [ ] The scheduler and run logic depend on a `Target` interface and a clock interface; tests inject fakes.

---

# 04: Tracer bullet: scheduled and manual runs in a local herdr tab

**What to build:** A task on `local` fires on schedule (or with `herdrcron run <id>`) and starts Claude Code with its prompt in a new `hc-` tab in a per-repo `hc-` workspace. The daemon polls the pane, maps the agent status to `running`, `blocked`, `done` or `lost`, and at the end stores the last 200 lines in `output_tail`. This is the first end-to-end path: task file → scheduler → herdr → run history.

**Blocked by:** 01, 03

**Status:** ready-for-agent

- [ ] Run steps 1–9 from the spec on `local`: reserve, preflight (cached 10 min), workspace, tab, prompt file, launch, track, finish.
- [ ] Commands are built with a POSIX shell quoter; tests show a prompt with quotes, `$`, backticks and newlines reaches the agent unchanged.
- [ ] One poll call per target per `poll_interval` covers all active runs on that target.
- [ ] `done`/`idle` needs 2 polls in a row; unknown status stays `running`; pane gone → `lost`.
- [ ] `on_done: close_tab` closes the tab; the remote prompt file is deleted at the end.
- [ ] `tasks.run_now`, `runs.list` and `herdrcron run <id>` work through the API.
- [ ] State-machine tests with a scripted fake herdr: working → done; working → blocked → working → done; pane vanishes.
- [ ] Manual check: a task with `*/2 * * * *` starts a Claude Code run in a new herdr tab every 2 minutes.

---

# 05: Stop and timeout

**What to build:** The user can stop an active run, and a run past its timeout is stopped automatically and shows `timeout`. With `on_blocked: stop`, a blocked run is stopped.

**Blocked by:** 04

**Status:** ready-for-agent

- [ ] Stop sequence: Ctrl-C twice, 2 s apart; wait 10 s; close the tab; output tail is saved first.
- [ ] `runs.stop` sets `stopped`; timeout sets `timeout` with `exit_reason` like `timeout after 2h`.
- [ ] `on_blocked: stop` stops the run on the first `blocked` status.
- [ ] Fake-clock tests cover timeout and user stop.

---

# 06: Overlap modes and per-target concurrency

**What to build:** When a fire time arrives while a run is still active, the task follows its `overlap` setting, and each target never runs more than `max_parallel` runs at once.

**Blocked by:** 04

**Status:** ready-for-agent

- [ ] `skip` logs one line and records nothing; `queue` keeps at most one pending fire and starts it when the active run ends; `parallel` starts a new run in a new tab.
- [ ] One worker per target; fires above `max_parallel` (default 4) wait in a per-target queue.
- [ ] `tasks.run_now` follows `overlap`; with `skip` and an active run it returns a distinct error unless `force_parallel` is set.
- [ ] Unit tests for each mode and for the concurrency limit.

---

# 07: Restart recovery and missed fires

**What to build:** Killing and restarting the daemon during a run neither loses the run nor starts a duplicate. Fires missed while the daemon was down follow `missed`.

**Blocked by:** 04

**Status:** ready-for-agent

- [ ] On start, every run without `ended_at` is looked up on its target: pane exists → tracking continues; pane gone → `lost`; target unreachable → run stays open and is retried.
- [ ] `missed: run_once` starts one `catch_up` run no matter how many fires were missed; fires older than `catch_up_window` are ignored; `missed: skip` does nothing.
- [ ] Recovery test: kill during a run, restart, assert one run row, tracking continues, no second run.
- [ ] Manual check: restarting the daemon during a real local run keeps tracking it.

---

# 08: Enable, disable, pause, resume, pause all

**What to build:** From the CLI, the user can turn a task on or off, pause it for a time or indefinitely, resume it, and pause or resume all tasks on one target or everywhere. Changes are written to the task file on disk.

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] API: `tasks.set_enabled`, `tasks.pause`, `tasks.resume`, `pause_all`, `resume_all`; CLI: `enable`, `disable`, `pause [--until]`, `resume`.
- [ ] Task files are written atomically (temp file, then rename).
- [ ] Paused fire times pass without a run and without catch-up; when `paused_until` passes, the daemon clears it and writes the file.
- [ ] Global pause lives in daemon state, not in task files, and shows in `status`.
- [ ] `next_fire_at` is recomputed on enable and resume.
- [ ] Unit tests for pause expiry and global pause.

---

# 09: Task CRUD, hand-edit reload and the `subscribe` stream

**What to build:** API clients can create, read, update and delete tasks with their prompt text, and receive live events. A hand edit of a task file reloads that task and reaches subscribers within 2 s. This is everything the TUI needs from the daemon.

**Blocked by:** 05, 08

**Status:** ready-for-agent

- [ ] `tasks.get` (definition, prompt, last 20 runs), `tasks.create`, `tasks.update`, `tasks.delete`, `reload`.
- [ ] Validation errors come back per field.
- [ ] `version` is a hash of the task file; `tasks.update` refuses on mismatch.
- [ ] `tasks.delete` refuses while a run is active unless `stop_active` is set; it removes task and prompt files; run history stays.
- [ ] fsnotify on `tasks/` reloads a changed file and emits `task.changed`.
- [ ] `subscribe` streams `task.changed`, `run.changed` and `target.changed`.
- [ ] Edits apply to the next run only; an active run is not changed.

---

# 10: Notifications

**What to build:** The user gets a notification through the configured command when a run is blocked, fails, times out, is lost, or a target becomes unreachable.

**Blocked by:** 05

**Status:** ready-for-agent

- [ ] `notify.command` is run as argv with the message appended; no shell.
- [ ] Only events listed in `notify.on` notify.
- [ ] A blocked run notifies once per change to `blocked`, not on every poll.
- [ ] A failing notify command is logged and does not affect the run.
- [ ] Manual check: a run that needs a permission shows `blocked` and sends one notification.

---

# 11: Log rotation, `herdrcron logs` and service install

**What to build:** The daemon runs as a user service that survives logout and reboot, and the user can read its log from the CLI.

**Blocked by:** 02

**Status:** ready-for-agent

- [ ] `daemon.log` rotates at 10 MB, keeps 5 files; every run line carries `run_id`, `task_id`, `target`.
- [ ] `herdrcron logs [-f] [--task <id>]` prints and filters the log.
- [ ] `herdrcron install-service` writes and enables a systemd user unit (`Restart=on-failure`, prints the `loginctl enable-linger` hint) or a launchd agent (`KeepAlive`).

---

# 12: TUI main screen with one-key actions

**What to build:** `herdrcron` with no subcommand opens a TUI listing all tasks on all targets, updated live from `subscribe`. The user can toggle, pause, resume, pause all, run now and stop from one key, with confirmation for destructive actions. Quitting the TUI leaves the daemon and all runs untouched.

**Blocked by:** 09

**Status:** ready-for-agent

- [ ] Columns, header and banner as in the spec mockup; sort blocked, then running, then `next_fire_at`; `g` groups by target; `/` filters.
- [ ] Colours: blocked warning, failed/timeout critical, running accent, off/paused dim.
- [ ] Keys `space`, `p` (menu: 1 h, tomorrow 08:00, date, indefinitely; resume on a paused task), `P`, `r` (asks to confirm parallel when needed), `s` (confirm), `q`, `?`.
- [ ] A hand edit of a task file shows in the open TUI within 2 s.
- [ ] `teatest` golden-file test for the main screen.
- [ ] Works over `ssh -t host herdrcron`.

---

# 13: TUI task detail, live output and attach (local)

**What to build:** From the main screen the user can open a task's detail, read past run output, watch a run's live output, and attach to a running local agent and come back.

**Blocked by:** 12

**Status:** ready-for-agent

- [ ] Detail: definition, next 5 fire times, last 20 runs; `enter` on a run shows its `output_tail`.
- [ ] `l` polls `runs.output` every 2 s and shows the last screen with ANSI kept.
- [ ] `runs.attach_cmd` returns the attach argv; `a` suspends the TUI, runs it, and resumes on detach.
- [ ] `attach_mode: full` opens the full herdr UI instead.

---

# 14: TUI task form, prompt editing and delete

**What to build:** The user can add and edit tasks in a form, edit the prompt in `$EDITOR`, and delete tasks, all from the TUI, and the changes land in the task files.

**Blocked by:** 12

**Status:** ready-for-agent

- [ ] `n` and `e` open a form with all task fields; target is a picker from config.
- [ ] Schedule field shows the next 3 fire times as you type, and errors inline.
- [ ] Field errors from the daemon show next to the field; a version conflict is shown clearly.
- [ ] `E` opens the prompt in `$EDITOR` and saves it through the API.
- [ ] `d` confirms; with an active run it offers "stop and delete".
- [ ] `teatest` golden-file test for the form.

---

# 15: SSH targets: run and attach on a remote machine

**What to build:** A task on a target defined in `config.yaml` runs on that machine over SSH exactly like a local task, and `a` in the TUI attaches to it.

**Blocked by:** 04, 13

**Status:** ready-for-agent

- [ ] SSH `Target` uses the system `ssh` binary with the spec's BatchMode / ControlMaster / ConnectTimeout options and `~/.ssh/config`.
- [ ] Every remote command runs through the target's `shell` (default `bash -lc`) and uses `herdr_path` when set; all arguments are shell-quoted.
- [ ] `PutFile` writes the prompt via stdin to the remote cache path.
- [ ] Exit code 255 is reported as unreachable; other non-zero exits as command errors.
- [ ] Attach uses `ssh -t <alias> …`; `attach_mode: full` uses `herdr --remote <alias>`.
- [ ] Integration test (build tag `integration`) against a local sshd container.
- [ ] Manual check: the `*/2` task on a remote target starts there and `a` attaches to it.

---

# 16: Target reachability, preflight, backoff and catch-up

**What to build:** The daemon knows which targets are reachable, keeps tracking runs through network drops, records `host_unreachable` fires, and catches up with `missed: run_once` when the target comes back. `herdrcron check <target>` prints a preflight report.

**Blocked by:** 07, 15

**Status:** ready-for-agent

- [ ] Unreachable target: mark it, back off 30 s doubling to 5 min, resume tracking when it answers; unreachable time never makes a run `failed`; timeout still counts.
- [ ] A fire at an unreachable target records a `host_unreachable` run; `missed` applies when it answers.
- [ ] `targets.check` and `herdrcron check` report herdr server, herdr version vs `herdr_version`, agent command on PATH, and cwd.
- [ ] Emits `target.changed` and notifies on `target_unreachable`.
- [ ] State-machine test: target drops for 3 polls and returns.
- [ ] Manual check: with a remote target offline at fire time, the run is `host_unreachable` and runs once when the target is back.

---

# 17: TUI targets screen and reachability header

**What to build:** The TUI header shows each target's reachability, and `t` opens a targets screen with herdr version and last preflight, where `c` runs a check.

**Blocked by:** 12, 16

**Status:** ready-for-agent

- [ ] Header shows `●`/`○ unreachable` per target, updated from `target.changed`.
- [ ] Targets screen: name, reachability, herdr version (mismatch shown as warning), last preflight result; `c` runs `targets.check`.

---

# 18: Release builds, integration suite and acceptance run

**What to build:** Tagged releases produce single static binaries, the integration suite runs against a real herdr, and every acceptance criterion in the spec is checked on the user's machines.

**Blocked by:** 06, 10, 11, 14, 17

**Status:** ready-for-agent

- [ ] GoReleaser builds linux/amd64, linux/arm64, darwin/arm64 static binaries.
- [ ] Integration test with a real herdr on localhost and agent command `bash -c 'echo hi; sleep 5'`.
- [ ] README covers install, config, remote-machine requirements and the prompt-size decision from 01.
- [ ] All acceptance criteria from the spec are checked off, with notes for any that need the user's machines.
