# herdrcron

Schedules recurring coding-agent runs in herdr panes. See `herdrcron.md` for the spec.

## Develop

```sh
go build ./... && go vet ./... && go test -race ./...
go run ./cmd/herdrcron daemon   # foreground; logs JSON to $XDG_STATE_HOME/herdrcron/daemon.log
go run ./cmd/herdrcron list     # asks the running daemon
```

Config lives in `$XDG_CONFIG_HOME/herdrcron/` (default `~/.config/herdrcron/`): `config.yaml`, `tasks/<id>.yaml`, `prompts/<id>.md`. State (socket, log) lives in `$XDG_STATE_HOME/herdrcron/` (default `~/.local/state/herdrcron/`).
