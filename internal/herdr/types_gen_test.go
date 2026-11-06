package herdr_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/herdr/schemagen"
)

// schemaPath must match the //go:generate line in generate.go.
const schemaPath = "schema/herdr-0.9.1.json"

func TestGeneratedTypesAreUpToDate(t *testing.T) {
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	version, err := schemagen.VersionFromSchemaPath(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := schemagen.Generate(schema, schemagen.Options{Package: "herdr", HerdrVersion: version, Selection: schemagen.Herdrcron})
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
