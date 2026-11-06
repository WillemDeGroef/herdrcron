# herdr CLI findings (herdr 0.9.1)

Results of the spike for issue #1. Every herdr call in herdrcron is built on this document, not on the guessed commands in `herdrcron.md`. Checked on 2026-11-06 against herdr **0.9.1** (API protocol 22, schema version 1) with `herdr <cmd> --help`, `herdr api schema --json` and live runs in `hc-` labelled workspaces. The user chose 0.9.1 over the spec's 0.7.1, so `herdr_version` in `config.yaml` should default to `"0.9.1"`.

## General CLI contract

- API commands print one JSON object on stdout: `{"id": "cli:<group>:<cmd>", "result": {"type": "<variant>", ...}}`. The `result.type` names the variant of `ResponseResult` in the schema (`workspace_created`, `pane_list`, `ok`, ...).
- Errors from the server: JSON on **stderr**, exit status 1, `{"error": {"code": "...", "message": "..."}, "id": "..."}`. Codes seen: `pane_not_found`, `tab_not_found`, `workspace_not_found`, `agent_not_found`, `server_not_running`. Syntax errors exit 2.
- IDs are opaque and server-scoped: workspace `w15`, tab `w15:t2`, pane `w15:p2`. Closed tab and pane ids are not reused. Parse them from responses; never build them.
- Two exceptions print plain text, not JSON: `pane read` (the terminal text) and `pane run`/`pane send-keys`/`pane send-text` (nothing on success, exit 0).
- The CLI picks its server from `HERDR_SOCKET_PATH` (set inside herdr panes) or from `--session <name>`; without either it uses the default session socket `~/.config/herdr/herdr.sock`. The daemon must not pass a socket or session, so it always talks to the user's default session. Under systemd no `HERDR_*` variables are set, which is what we want.

## Workspaces, tabs and panes

| Intent | Command | Result |
| --- | --- | --- |
| List workspaces | `herdr workspace list` | `workspace_list`: `.result.workspaces[]` (`WorkspaceInfo`: `workspace_id`, `label`, `active_tab_id`, `tab_count`, `pane_count`, `agent_status`, ...) |
| Create workspace | `herdr workspace create --cwd <cwd> --label hc-<repo> --no-focus` | `workspace_created`: `.result.workspace`, `.result.tab`, `.result.root_pane` |
| List tabs of a workspace | `herdr tab list --workspace <wid>` | `tab_list`: `.result.tabs[]` (`TabInfo`: `tab_id`, `label`, `pane_count`, `agent_status`, ...) |
| Create tab in a workspace | `herdr tab create --workspace <wid> --cwd <cwd> --label hc-<task>-<HHMM> --no-focus [--env K=V ...]` | `tab_created`: `.result.tab`, `.result.root_pane` |
| Rename a tab | `herdr tab rename <tab_id> <label>` | `tab_info` |
| Close a tab | `herdr tab close <tab_id>` | `ok`; a second close gives `tab_not_found` (exit 1) |
| List panes | `herdr pane list [--workspace <wid>]` | `pane_list`: `.result.panes[]` (`PaneInfo`) |
| One pane | `herdr pane get <pane_id>` | `pane_info`; `pane_not_found` (exit 1) once the pane is gone |
| Label a pane | `herdr pane rename <pane_id> <label>` | `pane_info` with `label` set |

Finding things by label (`hc-` prefix):

- Workspaces and tabs always have a `label` (a fresh tab without `--label` is labelled with its number, e.g. `"1"`). Filter `workspace list` / `tab list --workspace` on `label`.
- Panes have a `label` only after `pane rename`. `tab create --label` labels the tab, not its pane. herdrcron does not need pane labels: it stores the root pane id from `tab_created.root_pane.pane_id`, and can find a run's panes by `tab_id` in `pane list --workspace <wid>`.
- `WorkspaceInfo` has **no cwd field**, so the spec's "same cwd" check cannot be done on the workspace. Decision: find the workspace by label `hc-<repo-basename>` alone, and pass `--cwd` on every `tab create`, so the workspace cwd does not matter. Two repos with the same basename share one workspace; that is harmless, since each tab gets its own cwd.

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
| `unknown` | No agent detected, or herdr cannot classify it | stay `running` (timeout still applies); see below |

Observed sequence for `claude "<prompt>"` started with `pane run`: `unknown` (agent absent, shell) → `unknown` (agent `claude`, starting) → `working` → `idle`. A question UI (`AskUserQuestion`) gave `working` → `blocked`; after `esc` the status was `done`. herdr picks `idle` or `done` from its "seen" state, so herdrcron must treat both the same.

Rules for the tracker:

- Count `idle`/`done` towards completion only when `agent` is present. During startup the status is `unknown`, not `idle`, so a run does not finish before the agent starts.
- Pane missing from `pane list` → `lost`.
- `agent` absent after the agent has exited back to the shell (also what happens after a stop), or a plain command such as the integration test's `bash -c 'echo hi; sleep 5'`, gives `unknown` forever. To finish such runs, use `herdr pane process-info --pane <id>`: when `.result.process_info.foreground_process_group_id == shell_pid`, the shell is back at its prompt and the command has exited → `done`. While the command runs, the foreground group is the command's (`sleep`, `claude`, ...). Call it only for panes whose `agent` is absent, so the normal poll stays one `pane list` call per target.

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

## Headless herdr server

- API commands never start a server. With none running they fail with `server_not_running`, exit 1.
- `herdr server` starts a headless server and **detaches by itself**: the command prints the socket and log paths and returns at once, the server keeps running (capability `detached_server_daemon`). A second `herdr server` while one runs exits 1 immediately and changes nothing. Workspaces, tabs and `pane run` work in a headless server; the user can later attach with plain `herdr` (or `herdr --remote <alias>`).
- Tested with an isolated session (`herdr --session hc-issue1 server`, then `herdr --session hc-issue1 server stop` and `herdr session delete hc-issue1`), not the user's default server.

**Decision: preflight starts the server.** When a herdr call fails with `server_not_running`, preflight runs `herdr server </dev/null >/dev/null 2>&1` once (over SSH: through the target `shell`; the redirects stop the detached server from holding the SSH session open), then retries `herdr pane list`. If that still fails, the run is `failed` with the reason. Reason: the always-on host loses its herdr server on every reboot, and failing every run until the user opens a terminal defeats the scheduler. Starting a server is safe: it is the same server the user's `herdr` would start, and nothing else runs in it.

## Attach

`herdr agent attach <TARGET> [--takeover]` takes **a pane id or a unique agent name**, but only for a pane that hosts a detected agent. A plain shell pane gives `agent_not_found` (exit 1). It needs a TTY (`Not a tty` without one). Detach is the prefix key then `q` (default `ctrl+b q`, configurable in herdr's `config.toml`).

- Local argv: `["herdr", "agent", "attach", "<pane_id>"]`
- Remote argv: `["ssh", "-t", <ssh options>, "<alias>", "<shell> " + quote("herdr agent attach <pane_id>")]`, for the default shell `["ssh", "-t", "<alias>", "bash -lc 'herdr agent attach w1:p2'"]`, with `herdr_path` in place of `herdr` when set. `herdr --machine` does not forward interactive attach, so SSH is the way.
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
- For a new herdr version: `herdr api schema --json > internal/herdr/schema/herdr-<version>.json`, point the `go:generate` line at it, regenerate, and re-check this document.
