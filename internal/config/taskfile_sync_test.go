package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetEnabledKeepsTheFileWhenDirSyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.yaml")
	if err := os.WriteFile(path, []byte("id: x\nenabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := syncDirectory
	syncDirectory = func(string) error { return errors.New("eio") }
	t.Cleanup(func() { syncDirectory = orig })

	err := SetEnabled(path, false)
	if !errors.Is(err, ErrReplaced) {
		t.Fatalf("err = %v, want ErrReplaced", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(got), "enabled: false") {
		t.Fatalf("file after a failed directory sync:\n%s", got)
	}
	files, dirErr := os.ReadDir(dir)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	if len(files) != 1 {
		t.Fatalf("directory holds %d files, want only x.yaml", len(files))
	}
}

func TestWriteTaskCreateKeepsTheFileWhenDirSyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.yaml")
	orig := syncDirectory
	syncDirectory = func(string) error { return errors.New("eio") }
	t.Cleanup(func() { syncDirectory = orig })

	err := WriteTask(path, DefaultTask("x"), true)
	if !errors.Is(err, ErrReplaced) {
		t.Fatalf("err = %v, want ErrReplaced", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(got), "id: x") {
		t.Fatalf("file after a failed directory sync:\n%s", got)
	}
	files, dirErr := os.ReadDir(dir)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	if len(files) != 1 {
		t.Fatalf("directory holds %d files, want only x.yaml", len(files))
	}
}
