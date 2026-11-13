package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/WillemDeGroef/herdrcron/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestInsertGetRoundtrip(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	loc := time.FixedZone("CET", 3600)
	want := store.Run{
		ID:             store.NewRunID(time.Now()),
		TaskID:         "backup",
		Target:         "local",
		Trigger:        store.TriggerSchedule,
		ScheduledAt:    time.Date(2026, 1, 2, 3, 4, 5, 6, loc),
		StartedAt:      time.Date(2026, 1, 2, 3, 4, 6, 123456789, loc),
		EndedAt:        time.Date(2026, 1, 2, 4, 0, 0, 0, loc),
		Status:         store.StatusDone,
		HerdrWorkspace: "ws1",
		HerdrTab:       "tab1",
		HerdrPane:      "pane1",
		ExitReason:     "done",
		OutputTail:     "line1\nline2\n",
	}
	if err := s.InsertRun(ctx, want); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	got, err := s.GetRun(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	assertRunEqual(t, got, want)
	if got.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt location = %v, want UTC", got.StartedAt.Location())
	}

	manual := store.Run{
		ID:        store.NewRunID(time.Now()),
		TaskID:    "backup",
		Target:    "local",
		Trigger:   store.TriggerManual,
		StartedAt: time.Now(),
		Status:    store.StatusRunning,
	}
	if err := s.InsertRun(ctx, manual); err != nil {
		t.Fatalf("InsertRun manual: %v", err)
	}
	got, err = s.GetRun(ctx, manual.ID)
	if err != nil {
		t.Fatalf("GetRun manual: %v", err)
	}
	if !got.ScheduledAt.IsZero() || !got.EndedAt.IsZero() {
		t.Errorf("zero times not kept: scheduled %v, ended %v", got.ScheduledAt, got.EndedAt)
	}
	assertRunEqual(t, got, manual)
}

func TestInsertRunRequiresID(t *testing.T) {
	s := openStore(t)
	err := s.InsertRun(context.Background(), store.Run{TaskID: "a", StartedAt: time.Now()})
	if err == nil {
		t.Fatal("InsertRun without ID: want error")
	}
}

func TestGetRunMissing(t *testing.T) {
	s := openStore(t)
	_, err := s.GetRun(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun missing: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateRun(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	r := newRun("backup", time.Now())
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	r.Status = store.StatusTimeout
	r.EndedAt = r.StartedAt.Add(2 * time.Hour)
	r.ExitReason = "timeout after 2h"
	r.HerdrPane = "p9"
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	assertRunEqual(t, got, r)

	if err := s.UpdateRun(ctx, newRun("backup", time.Now())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateRun missing: err = %v, want ErrNotFound", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := newRun("backup", time.Now())
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	assertRunEqual(t, got, r)
}

func TestNewRunID(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 5 {
		ts := base.Add(time.Duration(i) * time.Second)
		id := store.NewRunID(ts)
		if len(id) != 26 {
			t.Fatalf("NewRunID length = %d, want 26 (%q)", len(id), id)
		}
		u, err := ulid.Parse(id)
		if err != nil {
			t.Fatalf("ulid.Parse(%q): %v", id, err)
		}
		if got := ulid.Time(u.Time()); !got.Equal(ts) {
			t.Errorf("ULID time = %v, want %v", got, ts)
		}
		ids = append(ids, id)
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("ids not in time order: %v", ids)
	}
}

func TestListRuns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Fractions .1 and .12 catch timestamps that do not sort as text.
	a1 := newRun("a", base.Add(100*time.Millisecond))
	a2 := newRun("a", base.Add(120*time.Millisecond))
	b1 := newRun("b", base.Add(time.Second))
	a3 := newRun("a", base.Add(time.Hour))
	for _, r := range []store.Run{a3, b1, a1, a2} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	tests := []struct {
		name  string
		task  string
		limit int
		want  []store.Run
	}{
		{"all", "", 0, []store.Run{a3, b1, a2, a1}},
		{"task a", "a", -1, []store.Run{a3, a2, a1}},
		{"task a limit", "a", 2, []store.Run{a3, a2}},
		{"task b", "b", 10, []store.Run{b1}},
		{"unknown task", "c", 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListRuns(ctx, tc.task, tc.limit)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ListRuns returned %d runs, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].ID != tc.want[i].ID {
					t.Errorf("run %d = %s, want %s", i, got[i].ID, tc.want[i].ID)
				}
			}
		})
	}
}

func TestListOpenRuns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	newer := newRun("a", base.Add(time.Hour))
	older := newRun("b", base.Add(100*time.Millisecond))
	ended := newRun("a", base)
	ended.EndedAt, ended.Status = base.Add(time.Minute), store.StatusDone
	for _, r := range []store.Run{newer, ended, older} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	got, err := s.ListOpenRuns(ctx)
	if err != nil {
		t.Fatalf("ListOpenRuns: %v", err)
	}
	if len(got) != 2 || got[0].ID != older.ID || got[1].ID != newer.ID {
		t.Fatalf("ListOpenRuns = %v, want %s then %s", got, older.ID, newer.ID)
	}
	assertRunEqual(t, got[0], older)
}

func TestLastRun(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if _, ok, err := s.LastRun(ctx, "a"); err != nil || ok {
		t.Fatalf("LastRun empty = ok %v, err %v; want false, nil", ok, err)
	}
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	older := newRun("a", base)
	newer := newRun("a", base.Add(time.Minute))
	other := newRun("b", base.Add(time.Hour))
	for _, r := range []store.Run{newer, older, other} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	got, ok, err := s.LastRun(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("LastRun = ok %v, err %v; want true, nil", ok, err)
	}
	assertRunEqual(t, got, newer)
}

func TestLastNonManualRun(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if _, ok, err := s.LastNonManualRun(ctx, "a"); err != nil || ok {
		t.Fatalf("LastNonManualRun empty = ok %v, err %v; want false, nil", ok, err)
	}
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	scheduled := newRun("a", base)
	scheduled.Trigger = store.TriggerSchedule
	scheduled.Status = store.StatusHostUnreachable
	manualNewer := newRun("a", base.Add(time.Minute))
	manualNewer.Trigger = store.TriggerManual
	manualNewer.Status = store.StatusDone
	for _, r := range []store.Run{scheduled, manualNewer} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	got, ok, err := s.LastNonManualRun(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("LastNonManualRun = ok %v, err %v; want true, nil", ok, err)
	}
	assertRunEqual(t, got, scheduled)
}

func TestTaskState(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if _, ok, err := s.GetTaskState(ctx, "a"); err != nil || ok {
		t.Fatalf("GetTaskState empty = ok %v, err %v; want false, nil", ok, err)
	}
	want := store.TaskState{
		TaskID:     "a",
		LastRunID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		NextFireAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.FixedZone("CEST", 7200)),
		LastFireAt: time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
	}
	if err := s.PutTaskState(ctx, want); err != nil {
		t.Fatalf("PutTaskState: %v", err)
	}
	assertTaskState(t, s, want)

	// Upsert: clear the next fire time and the last run.
	want.NextFireAt = time.Time{}
	want.LastRunID = ""
	if err := s.PutTaskState(ctx, want); err != nil {
		t.Fatalf("PutTaskState update: %v", err)
	}
	assertTaskState(t, s, want)
}

func TestDeleteRunsBefore(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ended := func(r store.Run) store.Run {
		r.EndedAt = r.StartedAt.Add(time.Minute)
		r.Status = store.StatusDone
		return r
	}
	oldEnded := ended(newRun("a", cutoff.Add(-48*time.Hour)))
	oldEnded2 := ended(newRun("b", cutoff.Add(-time.Nanosecond)))
	oldActive := newRun("a", cutoff.Add(-24*time.Hour))
	recent := ended(newRun("a", cutoff.Add(time.Hour)))
	atCutoff := ended(newRun("a", cutoff))
	for _, r := range []store.Run{oldEnded, oldEnded2, oldActive, recent, atCutoff} {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	for _, st := range []store.TaskState{
		{TaskID: "a", LastRunID: recent.ID},
		{TaskID: "b", LastRunID: oldEnded2.ID, LastFireAt: oldEnded2.StartedAt},
	} {
		if err := s.PutTaskState(ctx, st); err != nil {
			t.Fatalf("PutTaskState: %v", err)
		}
	}

	n, err := s.DeleteRunsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteRunsBefore: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d runs, want 2", n)
	}
	for _, r := range []store.Run{oldEnded, oldEnded2} {
		if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("run %s still there (err %v)", r.ID, err)
		}
	}
	for _, r := range []store.Run{oldActive, recent, atCutoff} {
		if _, err := s.GetRun(ctx, r.ID); err != nil {
			t.Errorf("run %s gone: %v", r.ID, err)
		}
	}
	assertTaskState(t, s, store.TaskState{TaskID: "a", LastRunID: recent.ID})
	assertTaskState(t, s, store.TaskState{TaskID: "b", LastFireAt: oldEnded2.StartedAt})
}

func TestConcurrentInsert(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	const workers, perWorker = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				errs <- s.InsertRun(ctx, newRun("a", time.Now()))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	runs, err := s.ListRuns(ctx, "a", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != workers*perWorker {
		t.Errorf("got %d runs, want %d", len(runs), workers*perWorker)
	}
}

// assertTaskState checks the stored state of want.TaskID.
func assertTaskState(t *testing.T, s *store.Store, want store.TaskState) {
	t.Helper()
	got, ok, err := s.GetTaskState(context.Background(), want.TaskID)
	if err != nil || !ok {
		t.Fatalf("GetTaskState = ok %v, err %v; want true, nil", ok, err)
	}
	if got.TaskID != want.TaskID || got.LastRunID != want.LastRunID {
		t.Errorf("state = %+v, want %+v", got, want)
	}
	if !got.NextFireAt.Equal(want.NextFireAt) || got.NextFireAt.IsZero() != want.NextFireAt.IsZero() {
		t.Errorf("NextFireAt = %v, want %v", got.NextFireAt, want.NextFireAt)
	}
	if !got.LastFireAt.Equal(want.LastFireAt) {
		t.Errorf("LastFireAt = %v, want %v", got.LastFireAt, want.LastFireAt)
	}
}

// newRun returns a valid active run of task started at started.
func newRun(task string, started time.Time) store.Run {
	return store.Run{
		ID:        store.NewRunID(started),
		TaskID:    task,
		Target:    "local",
		Trigger:   store.TriggerSchedule,
		StartedAt: started,
		Status:    store.StatusRunning,
	}
}

// assertRunEqual compares runs field by field, times with Equal.
func assertRunEqual(t *testing.T, got, want store.Run) {
	t.Helper()
	times := []struct {
		name      string
		got, want time.Time
	}{
		{"ScheduledAt", got.ScheduledAt, want.ScheduledAt},
		{"StartedAt", got.StartedAt, want.StartedAt},
		{"EndedAt", got.EndedAt, want.EndedAt},
	}
	for _, tc := range times {
		if !tc.got.Equal(tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	got.ScheduledAt, got.StartedAt, got.EndedAt = time.Time{}, time.Time{}, time.Time{}
	want.ScheduledAt, want.StartedAt, want.EndedAt = time.Time{}, time.Time{}, time.Time{}
	if got != want {
		t.Errorf("run = %+v, want %+v", got, want)
	}
}

func TestSetNextFireAndRecordFireTouchOnlyTheirColumns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	next := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	if err := s.SetNextFire(ctx, "a", next); err != nil {
		t.Fatal(err)
	}
	assertTaskState(t, s, store.TaskState{TaskID: "a", NextFireAt: next})

	fired := next
	if err := s.RecordFire(ctx, "a", "01ARZ3NDEKTSV4RRFFQ69G5FAV", fired); err != nil {
		t.Fatal(err)
	}
	assertTaskState(t, s, store.TaskState{TaskID: "a", LastRunID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", NextFireAt: next, LastFireAt: fired})

	// Clearing the next fire keeps the last run and fire.
	if err := s.SetNextFire(ctx, "a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	assertTaskState(t, s, store.TaskState{TaskID: "a", LastRunID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", LastFireAt: fired})

	// RecordFire on a task without state creates it.
	if err := s.RecordFire(ctx, "b", "01ARZ3NDEKTSV4RRFFQ69G5FAW", fired); err != nil {
		t.Fatal(err)
	}
	assertTaskState(t, s, store.TaskState{TaskID: "b", LastRunID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", LastFireAt: fired})
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserVersionForTest(99); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err := store.Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open of a database with a newer schema: want error")
	}
}

func assertGlobalPause(t *testing.T, s *store.Store, wantAll bool, wantTargets ...string) {
	t.Helper()
	all, targets, err := s.GlobalPause(context.Background())
	if err != nil {
		t.Fatalf("GlobalPause: %v", err)
	}
	if all != wantAll || !slices.Equal(targets, wantTargets) {
		t.Errorf("GlobalPause = %v, %q; want %v, %q", all, targets, wantAll, wantTargets)
	}
}

func TestGlobalPausePerTarget(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	assertGlobalPause(t, s, false)
	for _, target := range []string{"vps", "local", "vps"} { // pausing twice is fine
		if err := s.SetGlobalPause(ctx, target, true); err != nil {
			t.Fatalf("SetGlobalPause(%q, true): %v", target, err)
		}
	}
	assertGlobalPause(t, s, false, "local", "vps")
	if err := s.SetGlobalPause(ctx, "vps", false); err != nil {
		t.Fatalf("SetGlobalPause(vps, false): %v", err)
	}
	assertGlobalPause(t, s, false, "local")
	if err := s.SetGlobalPause(ctx, "vps", false); err != nil { // resuming twice is fine
		t.Fatalf("SetGlobalPause(vps, false) again: %v", err)
	}
	assertGlobalPause(t, s, false, "local")
}

func TestGlobalPauseAllTargets(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for _, target := range []string{"vps", ""} {
		if err := s.SetGlobalPause(ctx, target, true); err != nil {
			t.Fatalf("SetGlobalPause(%q, true): %v", target, err)
		}
	}
	assertGlobalPause(t, s, true, "vps")
	if err := s.SetGlobalPause(ctx, "", false); err != nil {
		t.Fatalf("SetGlobalPause(\"\", false): %v", err)
	}
	assertGlobalPause(t, s, false) // resuming all clears the per-target pause too
}

func TestGlobalPauseSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, target := range []string{"", "vps"} {
		if err := s.SetGlobalPause(ctx, target, true); err != nil {
			t.Fatalf("SetGlobalPause(%q, true): %v", target, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	assertGlobalPause(t, s, true, "vps")
}

func TestQueuedFiresRoundtripInQueueOrder(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	pending := store.QueuedFire{TaskID: "a", Target: "local", Trigger: store.TriggerSchedule, ScheduledAt: at, Pending: true}
	manual := store.QueuedFire{TaskID: "b", Target: "mini", Trigger: store.TriggerManual}
	var err error
	if pending.ID, err = s.AddQueuedFire(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if manual.ID, err = s.AddQueuedFire(ctx, manual); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListQueuedFires(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.QueuedFire{pending, manual}; !slices.Equal(got, want) {
		t.Fatalf("ListQueuedFires = %+v, want %+v", got, want)
	}

	// A released fire goes behind the fires already waiting.
	released := pending
	released.Pending = false
	if released.ID, err = s.ReleaseQueuedFire(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListQueuedFires(ctx); !slices.Equal(got, []store.QueuedFire{manual, released}) {
		t.Fatalf("after release: %+v, want %+v", got, []store.QueuedFire{manual, released})
	}
	if _, err := s.ReleaseQueuedFire(ctx, pending.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("release of a gone fire: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteQueuedFire(ctx, manual.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteQueuedFire(ctx, manual.ID); err != nil {
		t.Errorf("second delete: %v, want nil", err)
	}
	if got, _ = s.ListQueuedFires(ctx); !slices.Equal(got, []store.QueuedFire{released}) {
		t.Fatalf("after delete: %+v, want %+v", got, []store.QueuedFire{released})
	}
}
