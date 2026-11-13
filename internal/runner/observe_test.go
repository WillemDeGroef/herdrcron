package runner

import (
	"log/slog"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// A reachability result for a target Reconfigure has already replaced is
// dropped: it must not mark the new target, and it must not notify.
func TestObserveDropsAResultForAReplacedTarget(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	var calls []TargetState
	cfg := config.Config{Targets: map[string]config.Target{"local": {Host: "old"}}}
	r := New(Options{
		Clock:   clk,
		Targets: map[string]target.Target{"local": target.NewLocal()},
		Config:  cfg,
		Log:     slog.New(slog.DiscardHandler),
		OnTargetChange: func(st TargetState) {
			calls = append(calls, st)
		},
	})
	old := r.targets["local"]
	next := config.Config{Targets: map[string]config.Target{"local": {Host: "new"}}}
	if deferred := r.Reconfigure(next, func(string, config.Target) target.Target { return target.NewLocal() }); len(deferred) != 0 {
		t.Fatalf("deferred = %q, want none", deferred)
	}

	r.observe(old, "local", nil, true)
	if len(calls) != 0 {
		t.Fatalf("OnTargetChange ran for the replaced target: %+v", calls)
	}
	st, ok := r.TargetState("local")
	if !ok || st.Reachability != ReachUnknown || st.Gen == old.gen {
		t.Fatalf("new target = %+v, %v; want a new generation still unknown", st, ok)
	}

	cur := r.targets["local"]
	r.observe(cur, "local", nil, true)
	if len(calls) != 1 || calls[0].Reachability != ReachReachable || calls[0].Gen != cur.gen {
		t.Fatalf("OnTargetChange = %+v, want one reachable report for generation %d", calls, cur.gen)
	}
}
