package herdr_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
)

// fixture decodes one testdata file into a generated type and checks a few
// values, so a schema change that breaks a real herdr response shows up.
type fixture struct {
	target func() any       // new value to decode into
	check  func(v any) bool // reports whether the decoded value is as captured
}

// fixtures maps every file in testdata to its decode target and checks.
var fixtures = map[string]fixture{
	"workspace_create.json": {
		func() any { return new(herdr.Response[herdr.WorkspaceCreatedResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.WorkspaceCreatedResult]).Result
			return r.Type == "workspace_created" && r.Workspace.WorkspaceID == "w15" &&
				r.Tab.TabID == "w15:t1" && r.RootPane.PaneID == "w15:p1"
		},
	},
	"tab_create.json": {
		func() any { return new(herdr.Response[herdr.TabCreatedResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.TabCreatedResult]).Result
			return r.Tab.TabID == "w15:t2" && r.RootPane.PaneID == "w15:p2" && r.Tab.Label == "hc-spike-0930"
		},
	},
	"workspace_list.json": {
		func() any { return new(herdr.Response[herdr.WorkspaceListResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.WorkspaceListResult]).Result
			return len(r.Workspaces) == 3 && r.Workspaces[0].ActiveTabID == "w2:t60" && r.Workspaces[0].TabCount == 4
		},
	},
	"tab_list.json": {
		func() any { return new(herdr.Response[herdr.TabListResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.TabListResult]).Result
			return len(r.Tabs) == 3 && r.Tabs[0].Number == 198 && r.Tabs[0].AgentStatus == herdr.AgentStatusIdle
		},
	},
	"pane_list.json": {
		func() any { return new(herdr.Response[herdr.PaneListResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.PaneListResult]).Result
			if len(r.Panes) == 0 {
				return false
			}
			p := r.Panes[0]
			return p.PaneID != "" && validStatus(p.AgentStatus) && p.Agent != nil && *p.Agent == "claude" &&
				p.Scroll != nil && p.Scroll.ViewportRows == 83
		},
	},
	"agent_list.json": {
		func() any { return new(herdr.Response[herdr.AgentListResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.AgentListResult]).Result
			return len(r.Agents) == 3 && r.Agents[0].StateChangeSeq == 5870 && r.Agents[0].PaneID == "w2:p68"
		},
	},
	"pane_process_info.json": {
		func() any { return new(herdr.Response[herdr.PaneProcessInfoResult]) },
		func(v any) bool {
			pi := v.(*herdr.Response[herdr.PaneProcessInfoResult]).Result.ProcessInfo
			return pi.PaneID == "w16:p1" && pi.ShellPID != nil && *pi.ShellPID == 2072829 && pi.TTY == nil &&
				len(pi.ForegroundProcesses) == 1 && pi.ForegroundProcesses[0].Name == "sleep" &&
				len(pi.ForegroundProcesses[0].Argv) == 2 && pi.ForegroundProcesses[0].Argv0 == nil
		},
	},
	"tab_close.json": {
		func() any { return new(herdr.Response[herdr.OkResult]) },
		func(v any) bool {
			r := v.(*herdr.Response[herdr.OkResult])
			return r.ID == "cli:tab:close" && r.Result.Type == "ok"
		},
	},
	"error_pane_not_found.json": {
		func() any { return new(herdr.ErrorResponse) },
		func(v any) bool {
			r := v.(*herdr.ErrorResponse)
			return r.Error.Code == "pane_not_found" && r.Error.Message == "pane w15:p99 not found"
		},
	},
}

func validStatus(s herdr.AgentStatus) bool {
	switch s {
	case herdr.AgentStatusIdle, herdr.AgentStatusWorking, herdr.AgentStatusBlocked, herdr.AgentStatusDone, herdr.AgentStatusUnknown:
		return true
	}
	return false
}

// TestFixturesDecodeIntoGeneratedTypes decodes each fixture strictly,
// rejecting fields the generated types do not know.
func TestFixturesDecodeIntoGeneratedTypes(t *testing.T) {
	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			v := f.target()
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !f.check(v) {
				t.Errorf("got %+v", v)
			}
		})
	}
}

// TestEveryFixtureIsCovered keeps testdata and the fixtures table in step, so
// a new capture is not forgotten.
func TestEveryFixtureIsCovered(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, ok := fixtures[filepath.Base(f)]; !ok {
			t.Errorf("%s has no entry in fixtures", f)
		}
	}
}
