package schemagen_test

import (
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/herdr/schemagen"
)

// doc wraps success_response $defs (given as a JSON object body) into a
// minimal herdr schema document.
func doc(defs string) []byte {
	return []byte(`{
  "protocol": 7,
  "schema_version": 1,
  "schemas": {
    "success_response": {
      "properties": {"id": {"type": "string"}, "result": {"$ref": "#/schemas/success_response/$defs/ResponseResult"}},
      "required": ["id", "result"],
      "type": "object",
      "$defs": {` + defs + `}
    }
  }
}`)
}

func gen(t *testing.T, schema []byte, sel schemagen.Selection) string {
	t.Helper()
	out, err := schemagen.Generate(schema, schemagen.Options{Package: "p", HerdrVersion: "9.9.9", Selection: sel})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return string(out)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("generated code missing:\n%s\n--- got:\n%s", want, got)
	}
}

func TestRequiredObjectBecomesStructWithValueFields(t *testing.T) {
	got := gen(t, doc(`"Thing": {
  "type": "object",
  "properties": {"thing_id": {"type": "string"}, "focused": {"type": "boolean"}},
  "required": ["thing_id", "focused"]
}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "type Thing struct {\n\tFocused bool   `json:\"focused\"`\n\tThingID string `json:\"thing_id\"`\n}")
}

func TestOptionalScalarIsValueWithOmitempty(t *testing.T) {
	got := gen(t, doc(`"Thing": {
  "type": "object",
  "properties": {"label": {"type": "string"}, "ready": {"type": "boolean"}},
  "required": []
}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "\tLabel string `json:\"label,omitempty\"`\n\tReady bool   `json:\"ready,omitempty\"`\n")
}

func TestNullableScalarBecomesPointer(t *testing.T) {
	got := gen(t, doc(`"Thing": {
  "type": "object",
  "properties": {"cwd": {"type": ["string", "null"]}, "shell_pid": {"type": ["integer", "null"], "format": "uint32", "minimum": 0}},
  "required": ["cwd"]
}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "\tCwd      *string `json:\"cwd\"`\n\tShellPID *uint32 `json:\"shell_pid,omitempty\"`\n")
}

func TestIntegerFormatsMapToSizedUnsignedTypes(t *testing.T) {
	got := gen(t, doc(`"Thing": {
  "type": "object",
  "properties": {
    "a": {"type": "integer", "format": "uint", "minimum": 0},
    "b": {"type": "integer", "format": "uint64", "minimum": 0, "default": 0},
    "c": {"type": "integer", "format": "uint32", "minimum": 0}
  },
  "required": ["a", "b", "c"]
}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "\tA uint   `json:\"a\"`\n\tB uint64 `json:\"b\"`\n\tC uint32 `json:\"c\"`\n")
}

func TestReferencedDefsAreEmittedTransitively(t *testing.T) {
	got := gen(t, doc(`
"Outer": {
  "type": "object",
  "properties": {
    "inner": {"$ref": "#/schemas/success_response/$defs/Inner"},
    "maybe": {"anyOf": [{"$ref": "#/schemas/success_response/$defs/Inner"}, {"type": "null"}]},
    "extra": {"$ref": "#/schemas/success_response/$defs/Inner"}
  },
  "required": ["inner", "maybe"]
},
"Inner": {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]},
"Unused": {"type": "object", "properties": {}, "required": []}`), schemagen.Selection{Defs: []string{"Outer"}})

	assertContains(t, got, "type Outer struct {\n\tExtra *Inner `json:\"extra,omitempty\"`\n\tInner Inner  `json:\"inner\"`\n\tMaybe *Inner `json:\"maybe\"`\n}")
	assertContains(t, got, "type Inner struct {\n\tX string `json:\"x\"`\n}")
	if strings.Contains(got, "Unused") {
		t.Errorf("unselected, unreferenced def was emitted:\n%s", got)
	}
}

func TestStringEnumBecomesNamedTypeWithConstants(t *testing.T) {
	got := gen(t, doc(`
"Thing": {"type": "object", "properties": {"status": {"$ref": "#/schemas/success_response/$defs/AgentStatus"}}, "required": []},
"AgentStatus": {"enum": ["idle", "recent_unwrapped", "id"], "type": "string"}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "\tStatus AgentStatus `json:\"status,omitempty\"`\n")
	assertContains(t, got, "type AgentStatus string\n")
	assertContains(t, got, "const (\n\tAgentStatusIdle            AgentStatus = \"idle\"\n\tAgentStatusRecentUnwrapped AgentStatus = \"recent_unwrapped\"\n\tAgentStatusID              AgentStatus = \"id\"\n)")
}

func TestStringMapsArraysAndConsts(t *testing.T) {
	got := gen(t, doc(`
"Thing": {
  "type": "object",
  "properties": {
    "tokens": {"type": "object", "additionalProperties": {"type": "string"}, "maxProperties": 32, "propertyNames": {"pattern": "^[a-z]+$"}},
    "argv": {"type": ["array", "null"], "items": {"type": "string"}},
    "items": {"type": "array", "items": {"$ref": "#/schemas/success_response/$defs/Inner"}},
    "type": {"const": "thing", "type": "string"}
  },
  "required": ["items", "type"]
},
"Inner": {"type": "object", "properties": {}, "required": []}`), schemagen.Selection{Defs: []string{"Thing"}})

	assertContains(t, got, "type Thing struct {\n"+
		"\tArgv   []string          `json:\"argv,omitempty\"`\n"+
		"\tItems  []Inner           `json:\"items\"`\n"+
		"\tTokens map[string]string `json:\"tokens,omitempty\"`\n"+
		"\tType   string            `json:\"type\"`\n}")
}

func TestUnknownConstructsFailLoudly(t *testing.T) {
	cases := map[string]string{
		"unknown keyword": `{"type": "string", "allOf": [{"type": "string"}]}`,
		"oneOf property":  `{"oneOf": [{"type": "string"}, {"type": "boolean"}]}`,
		"number":          `{"type": "number"}`,
		"type union":      `{"type": ["string", "integer"]}`,
		"anyOf of two":    `{"anyOf": [{"type": "string"}, {"type": "boolean"}]}`,
		"inline object":   `{"type": "object", "properties": {"x": {"type": "string"}}}`,
		"integer format":  `{"type": "integer", "format": "float"}`,
		"non-string map":  `{"type": "object", "additionalProperties": true}`,
		"unknown ref":     `{"$ref": "#/schemas/success_response/$defs/Missing"}`,
	}
	for name, prop := range cases {
		t.Run(name, func(t *testing.T) {
			schema := doc(`"Thing": {"type": "object", "properties": {"p": ` + prop + `}, "required": ["p"]}`)
			_, err := schemagen.Generate(schema, schemagen.Options{Package: "p", Selection: schemagen.Selection{Defs: []string{"Thing"}}})
			if err == nil {
				t.Fatal("Generate succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "Thing") {
				t.Errorf("error %q does not say where (Thing)", err)
			}
		})
	}
}

func TestUnknownSelectedDefFails(t *testing.T) {
	_, err := schemagen.Generate(doc(``), schemagen.Options{Package: "p", Selection: schemagen.Selection{Defs: []string{"Nope"}}})
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("err = %v, want an error naming Nope", err)
	}
}

const resultDefs = `
"ResponseResult": {"oneOf": [
  {"type": "object", "properties": {"type": {"const": "pong", "type": "string"}}, "required": ["type"]},
  {"type": "object", "properties": {"type": {"const": "pane_read", "type": "string"}, "read": {"$ref": "#/schemas/success_response/$defs/PaneReadResult"}}, "required": ["type", "read"]},
  {"type": "object", "properties": {"type": {"const": "workspace_list", "type": "string"}, "workspaces": {"type": "array", "items": {"$ref": "#/schemas/success_response/$defs/PaneReadResult"}}}, "required": ["type", "workspaces"]}
]},
"PaneReadResult": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}`

func TestResultVariantsAreSelectedByTypeConst(t *testing.T) {
	got := gen(t, doc(resultDefs), schemagen.Selection{Results: []schemagen.Result{{Type: "workspace_list"}}})

	assertContains(t, got, "type WorkspaceListResult struct {\n\tType       string           `json:\"type\"`\n\tWorkspaces []PaneReadResult `json:\"workspaces\"`\n}")
	if strings.Contains(got, "Pong") {
		t.Errorf("unselected variant emitted:\n%s", got)
	}
}

func TestResultVariantNameCollisionFailsUnlessRenamed(t *testing.T) {
	sel := schemagen.Selection{Results: []schemagen.Result{{Type: "pane_read"}}}
	_, err := schemagen.Generate(doc(resultDefs), schemagen.Options{Package: "p", Selection: sel})
	if err == nil || !strings.Contains(err.Error(), "PaneReadResult") {
		t.Fatalf("err = %v, want a name collision on PaneReadResult", err)
	}

	sel.Results[0].Name = "PaneReadOutputResult"
	got := gen(t, doc(resultDefs), sel)
	assertContains(t, got, "type PaneReadOutputResult struct {\n\tRead PaneReadResult `json:\"read\"`\n\tType string         `json:\"type\"`\n}")
}

func TestUnknownResultTypeFails(t *testing.T) {
	sel := schemagen.Selection{Results: []schemagen.Result{{Type: "nope"}}}
	_, err := schemagen.Generate(doc(resultDefs), schemagen.Options{Package: "p", Selection: sel})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want an error naming nope", err)
	}
}

const errorSchema = `"error_response": {
  "title": "ErrorResponse",
  "type": "object",
  "properties": {"error": {"$ref": "#/schemas/error_response/$defs/ErrorBody"}, "id": {"type": "string"}},
  "required": ["id", "error"],
  "$defs": {"ErrorBody": {"type": "object", "properties": {"code": {"type": "string"}, "message": {"type": "string"}}, "required": ["code", "message"]}}
}`

func TestEnvelopesAndHeader(t *testing.T) {
	schema := strings.Replace(string(doc(``)), `"schemas": {`, `"schemas": {`+errorSchema+`,`, 1)
	got := gen(t, []byte(schema), schemagen.Selection{Envelopes: true})

	assertContains(t, got, "// Code generated by herdr-schemagen; DO NOT EDIT.\n")
	assertContains(t, got, "herdr 9.9.9 API schema (protocol 7, schema_version 1)")
	assertContains(t, got, "type Response[T any] struct {\n\tID     string `json:\"id\"`\n\tResult T      `json:\"result\"`\n}")
	assertContains(t, got, "type ErrorResponse struct {\n\tError ErrorBody `json:\"error\"`\n\tID    string    `json:\"id\"`\n}")
	assertContains(t, got, "type ErrorBody struct {\n\tCode    string `json:\"code\"`\n\tMessage string `json:\"message\"`\n}")
}

func TestGenerateIsDeterministic(t *testing.T) {
	sel := schemagen.Selection{Defs: []string{"PaneReadResult"}, Results: []schemagen.Result{{Type: "workspace_list"}, {Type: "pong"}}}
	first := gen(t, doc(resultDefs), sel)
	for i := 0; i < 20; i++ {
		if again := gen(t, doc(resultDefs), sel); again != first {
			t.Fatalf("run %d differs:\n%s\n--- first:\n%s", i, again, first)
		}
	}
}

func TestVersionFromSchemaPath(t *testing.T) {
	if got, err := schemagen.VersionFromSchemaPath("internal/herdr/schema/herdr-0.9.1.json"); err != nil || got != "0.9.1" {
		t.Errorf("got %q, %v; want 0.9.1", got, err)
	}
	if _, err := schemagen.VersionFromSchemaPath("schema.json"); err == nil {
		t.Error("want an error for a path not named herdr-<version>.json")
	}
}
