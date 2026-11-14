# herdr CLI findings (herdr 0.9.1)

Results of the spike for issue #1. Every herdr call in herdrcron is built on this document, not on the guessed commands in `herdrcron.md`. Checked on 2026-11-06 against herdr **0.9.1** (API protocol 22, schema version 1) with `herdr <cmd> --help`, `herdr api schema --json` and live runs in `hc-` labelled workspaces. The user chose 0.9.1 over the spec's 0.7.1, so `herdr_version` in `config.yaml` should default to `"0.9.1"`.

## General CLI contract

- API commands print one JSON object on stdout: `{"id": "cli:<group>:<cmd>", "result": {"type": "<variant>", ...}}`. The `result.type` names the variant of `ResponseResult` in the schema (`workspace_created`, `pane_list`, `ok`, ...).
- Errors from the server: JSON on **stderr**, exit status 1, `{"error": {"code": "...", "message": "..."}, "id": "..."}`. Codes seen: `pane_not_found`, `tab_not_found`, `workspace_not_found`, `agent_not_found`, `server_not_running`. Syntax errors exit 2.
- IDs are opaque and server-scoped: workspace `w15`, tab `w15:t2`, pane `w15:p2`. Closed tab and pane ids are not reused. Parse them from responses; never build them.
- Two exceptions print plain text, not JSON: `pane read` (the terminal text) and `pane run`/`pane send-keys`/`pane send-text` (nothing on success, exit 0).
- The CLI picks its server from `HERDR_SOCKET_PATH` (set inside herdr panes) or from `--session <name>`; without either it uses the default session socket `~/.config/herdr/herdr.sock`. The daemon must not pass a socket or session, so it always talks to the user's default session. Under systemd no `HERDR_*` variables are set, which is what we want. A daemon started by hand inside a herdr pane inherits them, so the local target drops every `HERDR_*` variable from the commands it runs.

## Workspaces, tabs and panes

| Intent | Command | Result |
| --- | --- | --- |
| List workspaces | `herdr workspace list` | `workspace_list`: `.result.workspaces[]` (`WorkspaceInfo`: `workspace_id`, `label`, `active_tab_id`, `tab_count`, `pane_count`, `agent_status`, ...) |
| Create workspace | `herdr workspace create --cwd <cwd> --label hc-<repo> --no-focus [--env K=V ...]` | `workspace_created`: `.result.workspace`, `.result.tab`, `.result.root_pane` |
| List tabs of a workspace | `herdr tab list --workspace <wid>` | `tab_list`: `.result.tabs[]` (`TabInfo`: `tab_id`, `label`, `pane_count`, `agent_status`, ...) |
| Create tab in a workspace | `herdr tab create --workspace <wid> --cwd <cwd> --label hc-<task>-<HHMM> --no-focus [--env K=V ...]` | `tab_created`: `.result.tab`, `.result.root_pane` |
| Rename a tab | `herdr tab rename <tab_id> <label>` | `tab_info` |
| Close a tab | `herdr tab close <tab_id>` | `ok`; a second close gives `tab_not_found` (exit 1) |
| List panes | `herdr pane list [--workspace <wid>]` | `pane_list`: `.result.panes[]` (`PaneInfo`) |
| One pane | `herdr pane get <pane_id>` | `pane_info`; `pane_not_found` (exit 1) once the pane is gone |
| Label a pane | `herdr pane rename <pane_id> <label>` | `pane_info` with `label` set |

Finding things by label (`hc-` prefix):

- Workspaces and tabs always have a `label` (a fresh tab without `--label` is labelled with its number, e.g. `"1"`). Filter `workspace list` / `tab list --workspace` on `label`.
- Panes have a `label` only after `pane rename`. `tab create --label` labels the tab, not its pane. To find a pane by an `hc-` label anyway: `herdr pane list [--workspace <wid>]` and match `.result.panes[].label` (there is no label filter flag; `label` is null until `herdr pane rename <pane_id> <label>`). herdrcron does not need pane labels: it stores the root pane id from `tab_created.root_pane.pane_id`, and finds a run's panes by `tab_id` in `pane list --workspace <wid>`.
- `WorkspaceInfo` has no `cwd` field. It only has `worktree.repo_root` for workspaces herdr opened as git worktrees (`herdr worktree ...`), which `workspace create` does not set, and pane `cwd` follows the shell's `cd`, so neither is a reliable workspace cwd. Decision (a deliberate change from the spec's "same cwd" check): find the workspace by label `hc-<repo-basename>` alone, and pass `--cwd` on every `tab create`, so the workspace cwd does not matter. Two repos with the same basename share one workspace; that is harmless, since each tab gets its own cwd.

Two behaviours the run logic must handle:

- `workspace create` also creates a first tab (label `"1"`) with a root pane. When herdrcron has just created the workspace, it should use that tab for the run and `tab rename` it to `hc-<task>-<HHMM>`, instead of leaving an empty tab behind.
- Closing the **last tab** of a workspace closes the workspace too (`workspace get` then returns `workspace_not_found`). So step 3 of a run must not cache the workspace id: look it up by label each time and create it when missing.

## Agent status

The field is **`agent_status`** on each pane in `herdr pane list` (`PaneInfo`). The same field exists on tabs and workspaces as a roll-up. The schema enum `AgentStatus` has exactly five values. The pane's `agent` field (string or absent) names the detected agent (`"claude"`); it is absent when herdr sees no agent in the pane.

| `agent_status` | Meaning in herdr | herdrcron run state |
| --- | --- | --- |
| `working` | Agent is busy | `running` |
| `blocked` | herdr recognised an approval or question UI | `blocked` (notify once on the change; `on_blocked: stop` stops the run) |
| `idle` | Agent ready for input, completion already seen | `done` after 2 polls in a row (debounce) |
| `done` | Agent ready for input, completion not yet seen by a client | `done` after 2 polls in a row (debounce) |
| `unknown` | No agent detected, or herdr cannot classify it | stay `running` (timeout still applies), unless the shell-at-prompt rule below ends the run |

Observed sequence for `claude "<prompt>"` started with `pane run`: `unknown` (agent absent, shell) → `unknown` (agent `claude`, starting) → `working` → `idle`. A question UI (`AskUserQuestion`) gave `working` → `blocked`; after `esc` the status was `done`. herdr picks `idle` or `done` from its "seen" state, so herdrcron must treat both the same.

Rules for the tracker:

- Count `idle`/`done` towards completion only when `agent` is present. During startup the status is `unknown`, not `idle`, so a run does not finish before the agent starts.
- Pane missing from `pane list` → `lost`.
- Pane in `pane list` with another `terminal_id` than at launch → `lost`, reason `pane replaced: herdr server restarted` (see "Server restart" below).
- `agent` absent gives `unknown`: before the agent starts, after it exits back to the shell, and for the whole run of a plain command such as the integration test's `bash -c 'echo hi; sleep 5'`. To tell whether the launched command has ended, use `herdr pane process-info --pane <id>`: when both `.result.process_info.shell_pid` and `foreground_process_group_id` are non-null and equal, the shell's own process group is in the foreground. After a command has run, that is the prompt (verified: `sleep` takes the foreground group; when it exits the two ids are equal again and `foreground_processes` is just the shell). The same equality is also true while startup files still run in the shell's group, before the command starts (observed: `conda` still listed in `foreground_processes`, ids already equal). The first sample after `pane run` must not by itself end the run. A later equal sample may: with a 15 s poll a short command can finish without a sample ever seeing the other process group. Call process-info only for panes whose `agent` is absent, so the normal poll stays one `pane list` call per target.
- Shell back at its prompt, not during a stop:
  - no agent was ever detected in the run (plain command) → `done`;
  - an agent was detected earlier and is now gone without herdrcron stopping it (crash, or the user typed `/exit`) → `failed` with reason `agent exited`.
- During a stop (step 10) skip these checks; the stop decides the state (`stopped` or `timeout`).

## Launch: `herdr pane run`

`herdr pane run <PANE_ID> <COMMAND>...` types the command text into the pane's shell and presses Enter, in one call. Several `COMMAND` words are joined with spaces, so pass the whole command line as **one** argv element. It prints nothing and exits 0 on success; `pane_not_found` on stderr, exit 1, for a bad pane.

Verified: a command string with double and single quotes, `$HOME` and backticks inside a single-quoted part reached the shell unchanged, and `claude "$(cat <prompt-path>)"` delivered a prompt with `"a" 'b' $HOME` and backticks unchanged to Claude Code.

The command text is typed into the user's interactive shell (here bash). The spec's form `cd <cwd> && env K=V <command> <args…> "$(cat <prompt-path>)"` needs a POSIX-like shell (bash, zsh; fish 3.x also accepts `&&` and `$(…)`). Both `cd` and `env` can move to `tab create --cwd <cwd> --env K=V`, which leaves only `<command> <args…> "$(cat <prompt-path>)"` in the typed text. Every element is still quoted with the POSIX quoter.

`herdr agent start <name> --kind claude --pane <id> -- <args>` is an alternative that waits until the agent is ready, but it only knows built-in agent kinds (not a custom `command`) and would put the prompt in herdr's own argv. We do not use it.

## Read output: `herdr pane read`

`herdr pane read <PANE_ID> [--source visible|recent|recent-unwrapped|detection] [--lines N] [--format text|ansi] [--ansi] [--raw]`

- `--source recent --lines N` works as the spec assumes: plain text on stdout, the last N rendered rows including the shell prompt line, no JSON envelope. The default source is `recent`.
- `recent` keeps soft wraps (a long line comes back as several rows). **Use `--source recent-unwrapped --lines 200` for `output_tail`**: it joins soft-wrapped lines, which is what a log needs.
- `--format ansi` (or `--ansi`) keeps colours, for the TUI's live output view. `--raw` keeps the raw bytes with `\r`.
- A missing pane gives `pane_not_found` on stderr, exit 1.
- `--lines` can only return what is on screen plus scrollback; full-screen apps (Claude Code's TUI) do not put everything in host scrollback, so long agent output can be cut.

## Stop

Verified on a blocked Claude Code run:

- `herdr pane send-keys <pane> ctrl+c` does **not** dismiss Claude Code's question dialog; `esc` does.
- Two separate `ctrl+c` calls 2 s apart did **not** exit Claude Code (the confirm window is shorter). One call with two keys, `herdr pane send-keys <pane> ctrl+c ctrl+c`, exits it to the shell (`agent` becomes absent).

Stop sequence for step 10: `send-keys <pane> esc`, then `send-keys <pane> ctrl+c ctrl+c`, wait 10 s, then `tab close <tab_id>`. Closing the tab kills whatever still runs in it, so the keys are only for a clean exit.

herdrcron sends `ctrl+c ctrl+c` a second time 2 s after the first (the spec's "Ctrl-C twice, 2 s apart"), then waits 10 s, reads the output tail and closes the tab. If the agent already exited, the second pair lands on the shell prompt and does nothing. A stop takes about 12 s.

## Headless herdr server

- API commands never start a server. With none running they fail with `server_not_running`, exit 1, JSON on stderr.
- `herdr server` **is** the headless server. It prints the socket and log paths to stderr and stays in the foreground until the process is stopped. It does not fork and exit. Verified on 0.9.1 with stdio redirected (`herdr --session <name> server </dev/null >/dev/null 2>&1` does not return) and on a pty. Workspaces, tabs and `pane run` work in that server; the user can later attach with plain `herdr` (or `herdr --remote <alias>`).
- `herdr status` reports capability `detached_server_daemon: true` only when that process is already its own session (the long-running user server). A foreground `herdr server` reports `false`. The flag does not mean the CLI daemonizes.
- A second `herdr server` while one is running exits 1 immediately and changes nothing. Its stderr is plain text, `error: herdr server is already running`, not the JSON error envelope.
- The process exits on SIGHUP, including when started under `nohup` (it installs its own handler). `setsid` still matters: a new session has no controlling terminal, so an SSH session closing does not deliver hangup. A signal sent to the pid does stop it.
- Tested on isolated sessions (`--session hc-issue1-review` and other `hc-issue1-*` names), then `herdr --session <name> server stop` and `herdr session delete <name>`. Not the user's default server.

**Decision: preflight starts the server, once, and does not wait for it.** When a herdr call fails with `server_not_running`:

- Local: the daemon spawns `herdr server` in a new session (Go `SysProcAttr.Setsid`), with stdin, stdout and stderr discarded, and does not wait.
- Remote, through the target shell (verified on Linux): `setsid herdr server </dev/null >/dev/null 2>&1 &`. `setsid` is what keeps the server out of the SSH session. Redirects alone do not; without the `&` the SSH command never returns.

Then retry `herdr pane list` until it succeeds or a few seconds have passed. If it still fails, the run is `failed` with the reason. Do not treat the starter's exit status as the result: the process is not supposed to exit, and a second start that races one already running exits 1 with the plain-text error above.

Reason: the always-on host loses its herdr server on every reboot, and failing every run until the user opens a terminal defeats the scheduler. This is the same server the user's `herdr` would start. The systemd unit that runs the daemon must not kill that process when the daemon stops (`KillMode=process`, or an equivalent). A server started in the daemon's cgroup dies with it, and takes the panes with it.

## Server restart

- A herdr server that stops (crash, `kill -9`, `herdr server stop`, reboot) takes every process in its panes with it. While it is down, `pane list` fails with `server_not_running`: the poll ends the target's runs `lost`.
- When the server starts again it restores the session from `session.json`: the same workspaces, tabs and labels, and the same pane ids (`w1:p1`), each a new shell in the old cwd. Only `terminal_id` differs (`term_66024df88e3dc1` before, `term_66024dff78c4b1` after). Verified on 0.9.1 with an isolated session (`hc-i34-exp`): `kill -9` of the server, then `herdr server` again.
- So a pane id alone does not identify a run's pane. If the server restarts between two polls, the poll finds the pane id and would track the empty shell: the run stays `running` (no agent was ever detected in that shell), or ends on a wrong reading.

**Decision: a run stores the pane's `terminal_id` at launch** (`runs.herdr_terminal`, from `root_pane` of `workspace create` / `tab create`). A poll, recovery at daemon start, and a stop or timeout that lands before the next poll, end the run `lost` when its pane id is listed with another `terminal_id`. They do not send keys to that shell or close its tab. Runs stored before this column existed have no terminal id and skip the check. The restored tab is left open, as for every `lost` run. A pane list that fails is not a replacement: a timeout still ends the run while the target does not answer.

## Attach

`herdr agent attach <TARGET> [--takeover]` takes **a pane id or a unique agent name**, but only for a pane that hosts a detected agent. A plain shell pane gives `agent_not_found` (exit 1). It needs a TTY (`Not a tty` without one). Detach is the prefix key then `q` (default `ctrl+b q`, configurable in herdr's `config.toml`).

- Local argv: `["herdr", "agent", "attach", "<pane_id>"]`
- Remote argv: `["ssh", "-t", <ssh options>, "<alias>", neutral("<shell> " + quote("herdr agent attach <pane_id>"))]`, with `herdr_path` in place of `herdr` when set. `neutral` (see "SSH: login shell and cancellation") makes the line read the same in any login shell; for the default shell it is `/bin/sh -c 'eval "$(printf %b "$0")"' 'bash -lc \0047herdr agent attach w1:p2\0047'`. `herdr --machine` does not forward interactive attach, so SSH is the way.
- `attach_mode: full`: local `["herdr"]`; remote `["herdr", "--remote", "<alias>"]`.
- When the agent has exited (`agent` absent), attach fails; the TUI should fall back to the full UI (`attach_mode: full` form) for that run.

## Prompts above 128 KiB

Linux limits one argument to 131072 bytes including its terminating NUL (`MAX_ARG_STRLEN`): 131071 bytes pass, 131072 fail with `Argument list too long` (verified). With `"$(cat <prompt-path>)"` the prompt is never in herdr's or SSH's argv, but the shell in the pane passes it as one argument to the agent, so the limit applies there.

**Decision: refuse prompts larger than 131071 bytes** at task validation (`tasks.create`, `tasks.update` and when loading a task file), with a field error on the prompt. Reason: the `send-text` fallback does not avoid the limit (`herdr pane send-text <pane> <TEXT>` and `herdr agent prompt <target> <TEXT>` take the text as one argv element too, so a big prompt would have to be split into chunks), it would need a second launch path that waits for the agent's input box, and pasting 128 KiB into a TUI is fragile. A prompt that large is better kept in a file in the repo and referred to from a short prompt.

## Go types

Go types for the JSON outputs above are generated from the schema:

- Schema copy: `internal/herdr/schema/herdr-0.9.1.json` (`herdr api schema --json`).
- Generator: `internal/herdr/cmd/herdr-schemagen`; output `internal/herdr/types_gen.go`.
- Regenerate: `go generate ./internal/herdr/...`. A test fails when `types_gen.go` is out of date, and another decodes real CLI output (`internal/herdr/testdata/`) into the generated types with unknown fields disallowed.
- Generated results: `workspace_*`, `tab_*`, `pane_info`, `pane_list`, `pane_process_info` and `ok` for the calls above; `agent_info`/`agent_list` because `herdr agent get|list` add `state_change_seq` and `interactive_ready`, a possible later way to detect a finished turn; `pane_read` (`PaneReadOutputResult`) is the socket API form of `pane read` and is unused while the CLI prints text.
- For a new herdr version: `herdr api schema --json > internal/herdr/schema/herdr-<version>.json`, point the `go:generate` line at it, regenerate, and re-check this document.

## SSH: login shell and cancellation

Not verified against a real sshd (SSH is tested with fakes only): the fake `ssh` in `internal/target` runs the remote line with `sh`, optionally in its own session as sshd does.

- sshd hands the command line to the remote user's login shell. bash, zsh, fish and tcsh disagree on quoting: fish reads `\\` and `\'` inside single quotes as escapes, csh expands `!` and rejects a newline inside quotes. They agree on a single-quoted word without `'`, `!`, a newline or a backslash before `'` or `\`. So every line is `/bin/sh -c 'eval "$(printf %b "$0")"' '<script>'`, with each byte of the script outside printable ASCII, and `'`, `\` and `!`, written as `\0ooo` (octal, decoded by `printf %b`). The script is the old line, `<shell> ` + quote(command), read by `/bin/sh`.
- Killing the local `ssh` signals nothing on the remote: without a TTY the command only sees its stdin close. `ssh -tt` would fix that but alters stdin and stdout (CR LF, echo, control bytes in a prompt). Instead herdrcron keeps ssh's stdin open for the whole call and the script leaves a watcher reading it. When stdin closes, the watcher signals the script, which stops the command.
- Stdin data (an `Exec` with stdin, the prompt of `PutFile`) is streamed as it is read, encoded: lines of printable ASCII with every other byte, and `\`, as `\0ooo`, then a line `\.`. The script decodes it with `read` and `printf %b` (builtins; `head -c` in BusyBox reads ahead) into a temp file, and only a stream that reaches the `\.` line is complete. So the length need not be known up front, a closed connection is never taken for the end of the data, and the script reads all of it whatever the command does: unread stdin of any size cannot hide a cancel. The cost is that the command starts when stdin ends. For `Exec` the temp file is from `mktemp` in `$TMPDIR`, opened as the command's stdin and removed at once. For `PutFile` it is `mktemp` next to the destination (mode 0600) and renamed into place when complete; a cancelled or cut upload removes it and leaves the destination as it was. `PutFile` runs in `/bin/sh` only, not through the target's `shell`.
- Stopping the command: SIGTERM, then SIGKILL after a grace period (5s) to what is left. Which processes:
  - With `setsid` on the remote, the command runs in a process group of its own and the group is signalled. This reaches processes whose parent already exited, and cannot hit a reused PID while the script has not waited for the group's leader or the group still has a member. What is left: the command ended on SIGTERM, a process it started did not, and that one ends by itself in the last moments before the SIGKILL.
  - Without `setsid`, the script signals its own process group, but only when it leads that group (it replaced the login shell of the session OpenSSH created with `setsid`). It does not signal a group led by its parent: that parent may be an interactive user's login shell, and the group then holds jobs this command did not start. The script cannot tell when this group is empty, so it stays for the whole grace period.
  - With neither (no `setsid`, and a login shell such as fish that does not exec the script), it signals the command and its descendants one by one. It stops each process (SIGSTOP) before it lists that process's children, then sends SIGTERM and SIGCONT: a stopped parent can neither start its next command nor wait for a child, so no child's PID is reused before its signal. Before the SIGKILL it checks that the PID still has the same process group. It misses processes whose parent already exited.
  - Process data comes from `/proc/<pid>/stat` where there is a `/proc`, else from `ps -A -o pid= -o ppid= -o pgid=` (no `-p`: BusyBox `ps` has `-o` and not `-p`). A listing that is not three numbers is ignored, so a `ps` that prints its default table is not treated as a process list. With none of `setsid`, `/proc` and that `ps`, the script says so on stderr and a cancel signals only the command itself.
- The watcher holds neither stdout nor stderr, so a finished command ends the ssh call at once, and the script lets go of both before it waits for the grace period. The script kills the watcher when the command finishes.
- `setsid herdr server … &` (see "herdr server") is in its own session and survives both kills.
- The interactive attach (`ssh -t`) has no watcher: the TTY's hangup ends the remote command.
