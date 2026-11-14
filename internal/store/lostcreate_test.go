package store_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/store"
)

func TestLostCreatesAreStoredListedAndDeleted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "herdrcron.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tab := store.LostCreate{RunID: "02", TaskID: "deps", Target: "box", Host: "box.example", Workspace: "w1", Label: "hc-deps-0800", Known: []string{"w1:t1", "w1:t2"}}
	ws := store.LostCreate{RunID: "01", TaskID: "sync", Target: "local", Label: "hc-repo", Known: []string{}}
	for _, c := range []store.LostCreate{tab, ws} {
		if err := st.AddLostCreate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// It survives a restart.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := st.ListLostCreates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.LostCreate{ws, tab}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListLostCreates = %+v, want %+v", got, want)
	}

	if err := st.DeleteLostCreate(ctx, "01"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLostCreate(ctx, "no such run"); err != nil {
		t.Fatalf("deleting a missing lost create: %v", err)
	}
	got, err = st.ListLostCreates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.LostCreate{tab}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the delete: %+v, want %+v", got, want)
	}
}
