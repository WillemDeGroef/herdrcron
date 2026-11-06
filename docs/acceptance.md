# Acceptance run

How each acceptance criterion of `herdrcron.md` ("Testing and acceptance criteria") is verified, as of issue #15 (2026-11-06, herdr 0.9.1).

Each item is one of:

- **verified-by-test**: an automated test proves it, with fake herdr/SSH/clock where named. Runs in CI (`go test -race ./...`) unless it carries the `integration` tag.
- **verified-manually-here**: run by hand on the development machine, against an isolated throwaway herdr session, never the user's own herdr server.
- **needs-user**: needs the user's machines (real Claude Code, a real remote, a desktop notifier, a service manager). Not checked; the manual steps are below.

The spec says all criteria "must pass on the user's machines", so every item also has a short manual step for the final sign-off.

## Summary

| # | Criterion | Status |
| --- | --- | --- |
| 1 | `local` task with `*/2 * * * *` starts a Claude Code run in a new herdr tab every 2 minutes, with its prompt | verified-by-test (logic) + verified-manually-here (real herdr, plain command); **needs-user** with real Claude Code |
| 2 | Same task on a remote target starts there; `a` attaches | verified-by-test (fake ssh); **needs-user** |
| 3 | Closing the TUI stops no schedule or run | verified-by-test; needs-user sign-off |
| 4 | TUI toggle, pause, resume, add, edit, delete, run now change the task file | verified-by-test; needs-user sign-off |
| 5 | A hand edit shows in the open TUI within 2 s | verified-by-test (real daemon); needs-user sign-off |
| 6 | A run that needs a permission shows `blocked` and sends one notification | verified-by-test (fake herdr, fake notifier); **needs-user** |
| 7 | A run past its timeout is stopped and shows `timeout` | verified-by-test; needs-user sign-off |
| 8 | Restarting the daemon during a run starts no duplicate and keeps tracking | verified-by-test; needs-user sign-off |
| 9 | Remote offline at fire time → `host_unreachable`; `missed: run_once` runs once when it is back | verified-by-test (fake target); **needs-user** |
| 10 | A prompt with quotes, `$`, backticks and newlines reaches the agent unchanged | verified-by-test (real `/bin/sh`) + verified-manually-here (real herdr); remote **needs-user** |

## Criteria

### 1. `*/2` task on `local` starts a Claude Code run in a new herdr tab

- [x] verified-by-test:
  - `internal/schedule`: `TestCronInTaskTimezone`.
  - `internal/scheduler`: `TestRunLoopFiresOnTheFakeClock`.
  - `internal/daemon`: `TestScheduledFireShowsAsLastRunAndMovesNext`.
  - `internal/runner`: `TestLaunchCreatesWorkspaceAndRenamesItsFirstTab` (new `hc-` workspace and tab) and `TestPromptFileIsWrittenAndCommandReadsIt`.
- [x] verified-manually-here: `go test -tags integration -run RealHerdr ./cmd/herdrcron/ ./internal/runner/` passed on 2026-11-06 against herdr 0.9.1, in throwaway sessions `hc-it-daemon-<pid>` and `hc-it-<pid>`. Both sessions were stopped and deleted afterwards.
  - `TestRealHerdrDaemonRunsTask` (cmd/herdrcron): the real daemon with `bash -c 'echo hi; sleep 5'`. `herdrcron run` starts the run in a new herdr tab, the run ends `done`, `runs.output` holds `hi`, and `on_done: close_tab` closes the tab.
  - `TestRealHerdrRunEndsDone` (internal/runner): the same through the runner, with a tricky prompt.
- [ ] needs-user (real Claude Code, real 2-minute cadence):
  1. `herdr integration install claude`; `claude` is logged in; the daemon runs (`herdrcron install-service`, or `herdrcron daemon`).
  2. Create `~/.config/herdrcron/prompts/ac1.md` with `Reply with the word pong and stop.`
  3. Create `~/.config/herdrcron/tasks/ac1.yaml`: `id: ac1`, `target: local`, `cwd: <a repo>`, `schedule: "*/2 * * * *"`, `agent: {command: claude}`, `timeout: 10m`, `on_done: keep_tab`.
  4. Over 6 minutes, check that `herdrcron list` and the TUI show three runs about 2 minutes apart. Each run has its own tab `hc-ac1-<HHMM>` in workspace `hc-<repo>`, Claude got the prompt, and each run ends `done`.
  5. Delete the task (TUI `d`) and close the tabs.

### 2. The same task on a remote target starts there, and `a` attaches

- [x] verified-by-test (fake `ssh` binary on PATH, argv assertions; by human decision no test uses a real remote):
  - `internal/target`: `TestSSHExecArgvUsesTheSpecOptionsAndTheTargetShell`, `TestSSHExecQuotesEveryArgument`, `TestSSHPutFileWritesThePromptViaStdin`.
  - `internal/daemon`: `TestRealTargetsMakesAnSSHTargetForEveryRemoteTarget`, `TestAttachCmdOnARemoteTargetUsesSSHWithATTY`.
  - `internal/tui`: `TestAttachRunsTheAttachCommand`, `TestAttachFromOutputAndDetail`.
- [ ] needs-user:
  1. On the remote: herdr 0.9.1, `herdr integration install claude`, `claude` logged in, the repo cloned. From the always-on host, `ssh <host> true` works without a prompt.
  2. Add the target to `config.yaml` (`targets: {vps: {host: <alias>}}`), then run `herdrcron check vps`. Every item is ok.
  3. Copy task `ac1` from criterion 1 with `target: vps` and the remote `cwd`.
  4. At the next fire, the TUI shows the run on `vps`, and `herdr --remote <alias>` shows the `hc-ac1-<HHMM>` tab on the remote.
  5. In the TUI, select the task and press `a`. You land in the remote agent's pane (`ssh -t … herdr agent attach <pane>`). Detach, and the TUI comes back. Repeat with `attach_mode: full`, which opens `herdr --remote`.
  6. Optional automated check against a real sshd: `HERDRCRON_SSH_TEST_HOST=<alias> go test -tags integration -run SSHIntegration ./internal/target/` (`TestSSHIntegration`).

### 3. Closing the TUI does not stop any schedule or run

- [x] verified-by-test: `internal/tui`: `TestQuitLeavesDaemonAlone` (quitting sends only read calls), `cmd/herdrcron`: `TestTUIShowsHandEditWithin2s` (TUI against a real daemon, then quits). The daemon is a separate process; the TUI only talks to it over `daemon.sock`.
- [ ] needs-user sign-off: with task `ac1` running, open `herdrcron`, press `q`, wait for the next fire. A new run starts and the earlier run keeps its tab and status.

### 4. Toggle, pause, resume, add, edit, delete and run now work from the TUI and change the task file

- [x] verified-by-test, in two halves:
  - TUI to API (`internal/tui`): `TestSpaceTogglesEnabled`, `TestPauseMenu`, `TestPOnPausedTaskResumes`, `TestRunNow`, `TestNewFormCreatesWithDefaults`, `TestEditFormUpdatesKeepingVersionAndPrompt`, `TestDeleteAsksThenDeletes`.
  - API to disk (`internal/daemon`): `TestDisableAndEnableWriteTheTaskFile`, `TestPauseAndResumeWriteTheTaskFile`, `TestTasksCreateWritesTaskAndPromptFiles`, `TestTasksUpdateChangesTheFilesAndRefusesAStaleVersion`, `TestTasksDeleteRemovesTheFilesAndKeepsRunHistory`, `TestRunNowStartsAManualRun`.
- [ ] needs-user sign-off: in the TUI press `space`, `p` (pause 1h), `p` (resume), `n` (new task), `e` (edit), `r` (run now) and `d` (delete). After each key, `cat ~/.config/herdrcron/tasks/<id>.yaml` shows the change (`enabled`, `paused_until`, the new or edited file, the file gone).

### 5. A hand edit of a task file shows in the open TUI within 2 s

- [x] verified-by-test: `cmd/herdrcron`: `TestTUIShowsHandEditWithin2s` (real daemon, teatest, 2 s deadline). `internal/daemon`: `TestHandEditReachesSubscribersWithinTwoSeconds`.
- [ ] needs-user sign-off: with `herdrcron` open, change `schedule:` in `~/.config/herdrcron/tasks/<id>.yaml` in an editor and save. The TUI row shows the new schedule and next fire within 2 s.

### 6. A run that needs a permission shows `blocked` and sends one notification

- [x] verified-by-test (fake herdr with scripted statuses, notifier argv captured):
  - `internal/runner`: `TestRunGoesFromWorkingToBlockedAndBackToDone`, `TestBlockedNotifiesOncePerChangeToBlocked`, `TestRecoveredBlockedRunDoesNotNotifyAgain`.
  - `internal/daemon`: `TestBlockedRunShowsTaskAsBlocked`, `TestFailedRunRunsTheNotifyCommand`.
  - `internal/notify`: `TestCommandGetsMessageAppendedWithoutAShell`.
- [ ] needs-user:
  1. Set `notify: {command: ["notify-send", "herdrcron"]}` (or `ntfy publish <topic>`, or `osascript`) in `config.yaml`.
  2. Create a task with `agent: {command: claude}`, no `--permission-mode`, and the prompt `Create the file /tmp/hc-ac6.txt containing hi.`
  3. Press `r` in the TUI. When Claude asks for permission, the task shows `blocked`, and exactly one notification arrives.
  4. Answer in the pane (`a`). The run goes back to running and then to `done`, with no second notification.

### 7. A run past its timeout is stopped and shows `timeout`

- [x] verified-by-test: `internal/runner`: `TestTimeoutStopsTheRun`, `TestTimeoutNotifies`. `internal/daemon`: `TestTimeoutStopsTheRun`.
- [ ] needs-user sign-off: create a task with `agent: {command: bash, args: ["-c", "sleep 600"]}`, `timeout: 30s` and `on_done: keep_tab`, then press `r`. After about 30 s plus one poll, the run shows `timeout` and its tab closes about 12 s later (Ctrl-C twice, then close).

### 8. Restarting the daemon during a run does not start a duplicate and keeps tracking it

- [x] verified-by-test (simulated kill and restart, fake herdr): `internal/daemon`: `TestRecoveryAfterKillKeepsTrackingTheSameRun`. `internal/runner`: `TestRecoverContinuesTrackingARunningRun`.
- [ ] needs-user sign-off: start a long run (`r` on the task from criterion 7, with `timeout: 10m`), then `systemctl --user restart herdrcron` (on macOS, `launchctl kickstart -k gui/$UID/io.github.willemdegroef.herdrcron`). `herdrcron list` shows the same run id, still running, and only one tab exists for it.

### 9. Remote offline at fire time → `host_unreachable`; `missed: run_once` runs it once when back

- [x] verified-by-test (fake target): `internal/daemon`: `TestUnreachableTargetEndToEnd`. `internal/runner`: `TestFireAtUnreachableTargetIsHostUnreachable`, `TestTargetDropsForThreePollsAndReturns`. `internal/scheduler`: `TestHostUnreachableFiresAreCaughtUpOnceWhenTheTargetAnswers`.
- [ ] needs-user:
  1. Use the remote task from criterion 2 with `missed: run_once`.
  2. Take the remote offline before a fire (shut it down, or block it, e.g. `ssh` to a stopped VM). At the fire time the run shows `host_unreachable`, the target shows unreachable in the TUI header and on `t`, and a `target_unreachable` notification arrives if configured.
  3. Bring the remote back. Within 5 minutes (backoff cap) the target is reachable again, and exactly one `catch_up` run starts.

### 10. A prompt with quotes, `$`, backticks and newlines reaches the agent unchanged

- [x] verified-by-test: `internal/runner`: `TestPromptReachesAgentUnchanged` (real `/bin/sh`, the stand-in agent compares argv byte for byte). `internal/shellquote`: `TestRoundTripThroughShell`. `internal/target`: `TestSSHExecQuotesEveryArgument`, `TestSSHPutFileWritesThePromptViaStdin`.
- [x] verified-manually-here: `TestRealHerdrRunEndsDone` (integration) passes a prompt with `"a" 'b'`, `$HOME` and a backtick command through a real herdr pane, and it arrives unchanged. The herdr spike (`docs/herdr-findings.md`) verified the same with Claude Code.
- [ ] needs-user (remote): run a task on the remote with the prompt below, and check that Claude echoes it exactly. Prompt: ``Repeat exactly: "a" 'b' $HOME `uname` `` followed by a second line `line two`.

## Test-list items from the spec

| Spec test | Tests | Status |
| --- | --- | --- |
| cron and `@every` with timezones and Brussels DST | `internal/schedule`: `TestSpringForwardBrussels`, `TestFallBackBrussels`. `internal/scheduler`: `TestDSTSpringForwardBrussels`, `TestDSTFallBackBrussels` | verified-by-test |
| overlap modes | `internal/dispatch`: `TestSkipScheduledFireWhileActiveIsSkippedAndLoggedOnce`, `TestQueueKeepsOnePendingFireAndStartsItWhenTheRunEnds`, `TestParallelStartsASecondRun` | verified-by-test |
| missed policy and `catch_up_window` | `internal/scheduler`: `TestRestartCatchesUpMissedFiresOnce`, `TestMissedFiresOlderThanCatchUpWindowAreIgnored` | verified-by-test |
| pause expiry | `internal/scheduler`: `TestPauseExpiryIsReportedOnceAtPausedUntil`. `internal/daemon`: `TestDaemonClearsPausedUntilWhenItPasses` | verified-by-test |
| task validation (including the 131071-byte prompt limit) | `internal/config`: `TestValidationRules`, `TestValidateRefusesAPromptFileAboveTheArgumentLimit`. `internal/daemon`: `TestTasksCreateAndUpdateRefuseAPromptAboveTheArgumentLimit` | verified-by-test |
| shell quoting | `internal/shellquote`: `TestRoundTripThroughShell` | verified-by-test |
| state machine: working → done; working → blocked → working → done; pane vanishes; target drops 3 polls; timeout | `internal/runner`: `TestRunGoesFromWorkingToDone`, `TestRunGoesFromWorkingToBlockedAndBackToDone`, `TestPaneVanishesRunIsLost`, `TestTargetDropsForThreePollsAndReturns`, `TestTimeoutStopsTheRun` | verified-by-test |
| recovery after a kill | `internal/daemon`: `TestRecoveryAfterKillKeepsTrackingTheSameRun` | verified-by-test |
| integration: real herdr on localhost, `bash -c 'echo hi; sleep 5'` | `cmd/herdrcron`: `TestRealHerdrDaemonRunsTask` (tag `integration`) | verified-manually-here (not in CI: CI has no herdr) |
| integration: SSH target against a local sshd container | `internal/target`: `TestSSHIntegration` (tag `integration`, needs `HERDRCRON_SSH_TEST_HOST`) | **needs-user**: SSH is tested with fakes only, by human decision; no real sshd was started here |
| TUI teatest golden files: main screen and form | `internal/tui`: `TestMainScreenGolden`, `TestFormGolden` | verified-by-test |
| service install | `internal/service`: `TestLinuxUnitContent`, `TestLinuxRunsSystemctlInOrder`, `TestDarwinWritesPlist`. `cmd/herdrcron`: `TestInstallServiceRunsServiceManager` | verified-by-test (fake systemctl/launchctl); **needs-user**: run `herdrcron install-service` on each machine, then `systemctl --user status herdrcron` or `launchctl print gui/$UID/io.github.willemdegroef.herdrcron` |
| release builds | `.goreleaser.yaml`: `goreleaser check` and `goreleaser release --snapshot --clean` passed here. They built linux/amd64 and linux/arm64 (static ELF) and darwin/arm64 | verified-manually-here; the first tagged release is up to the user |
