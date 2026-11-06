package herdr_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/herdr/schemagen"
)

// TestGeneratedTypesAreUpToDate regenerates types_gen.go from the one
// checked-in schema (named in generate.go's //go:generate line) and compares.
func TestGeneratedTypesAreUpToDate(t *testing.T) {
	schemas, err := filepath.Glob("schema/herdr-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 1 {
		t.Fatalf("want exactly one schema/herdr-*.json, got %v", schemas)
	}
	want, err := schemagen.GenerateFile(schemas[0], "herdr")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("types_gen.go")
	if err != nil {
		t.Fatalf("%v; run `go generate ./internal/herdr/...`", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("types_gen.go is stale; run `go generate ./internal/herdr/...`")
	}
}
