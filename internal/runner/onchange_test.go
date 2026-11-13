package runner_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// changes records the runs OnChange is called with.
type changes struct {
	mu   sync.Mutex
	runs []store.Run
}

func (c *changes) add(r store.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, r)
}

func (c *changes) statuses(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.runs {
		if r.ID == id {
			out = append(out, r.Status)
		}
	}
	return out
}

func (e *env) withOnChange(onChange func(store.Run)) {
	e.runner = runner.New(runner.Options{
		Clock: e.clock, Store: e.store,
		Targets:         map[string]target.Target{"local": e.herdr},
		Config:          config.Config{Timezone: "Europe/Brussels", Targets: map[string]config.Target{"local": {}}},
		ConfigDir:       e.configDir,
		Log:             e.logger,
		ServerStartWait: 50 * time.Millisecond,
		OnChange:        onChange,
	})
}

func TestOnChangeReportsEveryStoredStatus(t *testing.T) {
	e := newEnv(t)
	var c changes
	e.withOnChange(c.add)
	e.herdr.script(working, blocked, working, doneS, doneS)
	run := e.start(deps())
	e.statuses(run.ID, 5)
	// Insert, herdr ids stored, running, blocked, running, done: one call
	// per stored change, with the stored status.
	want := []string{"starting", "starting", "running", "blocked", "running", "done"}
	if got := c.statuses(run.ID); !slices.Equal(got, want) {
		t.Fatalf("OnChange statuses = %v, want %v", got, want)
	}
}

func TestOnChangeReportsARunThatFailsToStart(t *testing.T) {
	e := newEnv(t)
	var c changes
	e.withOnChange(c.add)
	task := deps()
	task.Target = "nowhere"
	run := e.start(task)
	if want := []string{"starting", "failed"}; !slices.Equal(c.statuses(run.ID), want) {
		t.Fatalf("OnChange statuses = %v, want %v", c.statuses(run.ID), want)
	}
}

func TestLiveRunsIncludesARunStillStarting(t *testing.T) {
	e := newEnv(t)
	var live []store.Run
	var once sync.Once
	// OnChange for the stored herdr ids runs during the launch: the run is
	// starting and not yet in ActiveRuns, but LiveRuns has it.
	e.withOnChange(func(r store.Run) {
		if r.Status == store.StatusStarting && r.HerdrPane != "" {
			once.Do(func() { live = e.runner.LiveRuns() })
		}
	})
	e.herdr.script(working)
	run := e.start(deps())
	if len(live) != 1 || live[0].ID != run.ID || live[0].Status != store.StatusStarting {
		t.Fatalf("LiveRuns during launch = %+v, want the starting run", live)
	}
	if got := e.runner.LiveRuns(); len(got) != 1 || got[0].Status != store.StatusRunning {
		t.Fatalf("LiveRuns after launch = %+v, want the running run", got)
	}
}
