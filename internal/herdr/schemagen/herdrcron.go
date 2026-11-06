package schemagen

// Herdrcron is what herdrcron's herdr package uses from the herdr API.
// To use another type, add it here and run `go generate ./internal/herdr/...`.
var Herdrcron = Selection{
	Defs: []string{
		"AgentInfo",
		"AgentStatus",
		"PaneInfo",
		"PaneReadResult",
		"TabInfo",
		"WorkspaceInfo",
	},
	Results: []Result{
		{Type: "agent_info"},
		{Type: "agent_list"},
		{Type: "ok"},
		{Type: "pane_info"},
		{Type: "pane_list"},
		{Type: "pane_process_info"},
		{Type: "pane_read", Name: "PaneReadOutputResult"}, // PaneReadResult is the def it wraps
		{Type: "tab_created"},
		{Type: "tab_info"},
		{Type: "tab_list"},
		{Type: "workspace_created"},
		{Type: "workspace_info"},
		{Type: "workspace_list"},
	},
	Envelopes: true,
}
