package herdr_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
)

// decodeStrict decodes testdata/name into v, rejecting fields the generated
// types do not know.
func decodeStrict(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func validStatus(s herdr.AgentStatus) bool {
	switch s {
	case herdr.AgentStatusIdle, herdr.AgentStatusWorking, herdr.AgentStatusBlocked, herdr.AgentStatusDone, herdr.AgentStatusUnknown:
		return true
	}
	return false
}

func TestFixturesDecodeIntoGeneratedTypes(t *testing.T) {
	t.Run("workspace_create", func(t *testing.T) {
		var r herdr.Response[herdr.WorkspaceCreatedResult]
		decodeStrict(t, "workspace_create.json", &r)
		if r.Result.Type != "workspace_created" || r.Result.Workspace.WorkspaceID != "w15" ||
			r.Result.Tab.TabID != "w15:t1" || r.Result.RootPane.PaneID != "w15:p1" {
			t.Errorf("got %+v", r.Result)
		}
	})
	t.Run("tab_create", func(t *testing.T) {
		var r herdr.Response[herdr.TabCreatedResult]
		decodeStrict(t, "tab_create.json", &r)
		if r.Result.Tab.TabID != "w15:t2" || r.Result.RootPane.PaneID != "w15:p2" || r.Result.Tab.Label != "hc-spike-0930" {
			t.Errorf("got %+v", r.Result)
		}
	})
	t.Run("workspace_list", func(t *testing.T) {
		var r herdr.Response[herdr.WorkspaceListResult]
		decodeStrict(t, "workspace_list.json", &r)
		if len(r.Result.Workspaces) != 3 || r.Result.Workspaces[0].ActiveTabID != "w2:t60" || r.Result.Workspaces[0].TabCount != 4 {
			t.Errorf("got %+v", r.Result)
		}
	})
	t.Run("tab_list", func(t *testing.T) {
		var r herdr.Response[herdr.TabListResult]
		decodeStrict(t, "tab_list.json", &r)
		if len(r.Result.Tabs) != 3 || r.Result.Tabs[0].Number != 198 || r.Result.Tabs[0].AgentStatus != herdr.AgentStatusIdle {
			t.Errorf("got %+v", r.Result)
		}
	})
	t.Run("pane_list", func(t *testing.T) {
		var r herdr.Response[herdr.PaneListResult]
		decodeStrict(t, "pane_list.json", &r)
		if len(r.Result.Panes) == 0 {
			t.Fatal("no panes")
		}
		p := r.Result.Panes[0]
		if p.PaneID == "" || !validStatus(p.AgentStatus) || p.Agent == nil || *p.Agent != "claude" ||
			p.Scroll == nil || p.Scroll.ViewportRows != 83 {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("agent_list", func(t *testing.T) {
		var r herdr.Response[herdr.AgentListResult]
		decodeStrict(t, "agent_list.json", &r)
		if len(r.Result.Agents) != 3 || r.Result.Agents[0].StateChangeSeq != 5870 || r.Result.Agents[0].PaneID != "w2:p68" {
			t.Errorf("got %+v", r.Result)
		}
	})
	t.Run("pane_process_info", func(t *testing.T) {
		var r herdr.Response[herdr.PaneProcessInfoResult]
		decodeStrict(t, "pane_process_info.json", &r)
		pi := r.Result.ProcessInfo
		if pi.PaneID != "w16:p1" || pi.ShellPID == nil || *pi.ShellPID != 2072829 || pi.TTY != nil ||
			len(pi.ForegroundProcesses) != 1 || pi.ForegroundProcesses[0].Name != "sleep" ||
			len(pi.ForegroundProcesses[0].Argv) != 2 || pi.ForegroundProcesses[0].Argv0 != nil {
			t.Errorf("got %+v", pi)
		}
	})
	t.Run("tab_close", func(t *testing.T) {
		var r herdr.Response[herdr.OkResult]
		decodeStrict(t, "tab_close.json", &r)
		if r.ID != "cli:tab:close" || r.Result.Type != "ok" {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("error_pane_not_found", func(t *testing.T) {
		var r herdr.ErrorResponse
		decodeStrict(t, "error_pane_not_found.json", &r)
		if r.Error.Code != "pane_not_found" || r.Error.Message != "pane w15:p99 not found" {
			t.Errorf("got %+v", r)
		}
	})
}

// Every fixture must be covered above, so a new capture is not forgotten.
func TestEveryFixtureIsCovered(t *testing.T) {
	covered := map[string]bool{
		"workspace_create.json": true, "tab_create.json": true, "workspace_list.json": true,
		"tab_list.json": true, "pane_list.json": true, "agent_list.json": true,
		"pane_process_info.json": true, "tab_close.json": true, "error_pane_not_found.json": true,
	}
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !covered[filepath.Base(f)] {
			t.Errorf("%s has no decode test in TestFixturesDecodeIntoGeneratedTypes", f)
		}
	}
}
