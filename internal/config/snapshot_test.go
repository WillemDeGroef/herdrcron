package config_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
)

// SnapshotTasks versions the bytes it parsed. A write that lands afterwards
// changes Version and not the snapshot, so a reload cannot store the old
// task under the new hash.
func TestSnapshotTasksPinsTheBytesItParsed(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	f.task("peppol-sh-deps.yaml", validTask)
	taskPath := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")
	promptPath := filepath.Join(f.dir, "prompts", "peppol-sh-deps.md")

	entries, versions, err := config.SnapshotTasks(f.dir, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadTasks(f.dir, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(versions) != 1 {
		t.Fatalf("snapshot = %d entries, %d versions", len(entries), len(versions))
	}
	if entries[0].Err != nil {
		t.Fatal(entries[0].Err)
	}
	if !reflect.DeepEqual(entries[0].Task, loaded[0].Task) || entries[0].ID != loaded[0].ID || entries[0].File != loaded[0].File {
		t.Fatalf("snapshot entry = %+v, load = %+v", entries[0], loaded[0])
	}
	pinned, err := config.Version(taskPath, promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if versions[0] != pinned {
		t.Fatalf("snapshot version = %s, Version = %s", versions[0], pinned)
	}

	writeFile(t, taskPath, validTask+"\nname: edited after the read\n")
	writeFile(t, promptPath, "a different prompt\n")
	if got := entries[0].Task.Name; got != "peppol.sh dependency update" {
		t.Fatalf("name = %q, want the name from the bytes that were parsed", got)
	}
	now, err := config.Version(taskPath, promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if now == pinned {
		t.Fatal("rewriting the files did not change Version")
	}
	if versions[0] != pinned {
		t.Fatalf("snapshot version changed to %s after the write", versions[0])
	}
}

// An invalid file is still returned, and its version is the hash of the
// bytes that were read, prompt included, so the watcher can see a later edit.
func TestSnapshotTasksVersionsAnInvalidFile(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	body := "id: peppol-sh-deps\nenabeld: true\nprompt_file: prompts/peppol-sh-deps.md\ntimeout: 2h\nagent:\n  command: claude\ntarget: vps\nschedule: \"@daily\"\n"
	f.task("peppol-sh-deps.yaml", body)
	taskPath := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")
	promptPath := filepath.Join(f.dir, "prompts", "peppol-sh-deps.md")

	entries, versions, err := config.SnapshotTasks(f.dir, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Err == nil {
		t.Fatalf("entry = %+v, want an invalid file", entries)
	}
	want, err := config.Version(taskPath, promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if versions[0] != want {
		t.Fatalf("version = %s, want %s", versions[0], want)
	}
}

func TestSnapshotTasksSortsEntriesWithTheirVersions(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"b", "a"} {
		f.prompt(id + ".md")
		f.task(id+".yaml", "id: "+id+"\ntarget: local\nschedule: \"@daily\"\ntimeout: 1h\nagent:\n  command: claude\n")
	}
	entries, versions, err := config.SnapshotTasks(f.dir, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != "a" || entries[1].ID != "b" {
		t.Fatalf("ids = %q, %q, want a then b", entries[0].ID, entries[1].ID)
	}
	for i, e := range entries {
		got, err := config.Version(e.File, filepath.Join(f.dir, "prompts", e.ID+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if versions[i] != got {
			t.Fatalf("%s version = %s, want %s", e.ID, versions[i], got)
		}
	}
}
