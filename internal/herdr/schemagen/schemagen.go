package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Options configures one generator run.
type Options struct {
	Package      string    // Go package name of the generated file
	HerdrVersion string    // herdr version the schema came from, recorded in the header
	Selection    Selection // which schema types to emit
}

// Selection names the schema types to emit. Everything they reference is
// emitted too.
type Selection struct {
	// Defs are names of success_response $defs.
	Defs []string
	// Results are ResponseResult variants.
	Results []Result
	// Envelopes emits the generic success envelope Response[T] (checked
	// against success_response) and ErrorResponse (from error_response).
	Envelopes bool
}

// Result selects one variant of success_response's ResponseResult oneOf.
type Result struct {
	// Type is the variant's "type" const, e.g. "workspace_list".
	Type string
	// Name is the Go type name. Empty means CamelCase(Type) + "Result".
	Name string
}

// node is one JSON schema node.
type node struct {
	Type   typeList `json:"type"`
	Format string   `json:"format"`
	Ref    string   `json:"$ref"`
	AnyOf  []*node  `json:"anyOf"`
	Enum   []any    `json:"enum"`
	Const  any      `json:"const"`
	Items  *node    `json:"items"`
	// AdditionalProperties is only understood as a schema (not a bool).
	AdditionalProperties *node            `json:"additionalProperties"`
	Properties           map[string]*node `json:"properties"`
	Required             []string         `json:"required"`
}

// knownKeywords are the schema keywords the generator understands. The
// second group only constrains values and does not change the Go type, so it
// is accepted and ignored. Anything else is an error.
var knownKeywords = map[string]bool{
	"type": true, "format": true, "$ref": true, "anyOf": true, "enum": true, "const": true,
	"items": true, "additionalProperties": true, "properties": true, "required": true,

	"minimum": true, "maximum": true, "default": true, "description": true, "title": true,
	"maxProperties": true, "minProperties": true, "propertyNames": true, "pattern": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true, "$schema": true,
}

func (n *node) UnmarshalJSON(b []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	for k := range keys {
		if !knownKeywords[k] {
			return fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	type plain node // drops this method, avoiding recursion
	return json.Unmarshal(b, (*plain)(n))
}

// typeList holds the "type" keyword, which is a string or a list of strings.
type typeList []string

func (t *typeList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = typeList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("type: %w", err)
	}
	*t = many
	return nil
}

type document struct {
	Protocol      int                  `json:"protocol"`
	SchemaVersion int                  `json:"schema_version"`
	Schemas       map[string]topSchema `json:"schemas"`
}

// topSchema is one entry of the document's "schemas": a schema with $defs.
type topSchema struct {
	Defs  map[string]json.RawMessage `json:"$defs"`
	Title string                     `json:"title"`
	raw   json.RawMessage
}

func (t *topSchema) UnmarshalJSON(b []byte) error {
	type plain topSchema
	if err := json.Unmarshal(b, (*plain)(t)); err != nil {
		return err
	}
	t.raw = append(json.RawMessage(nil), b...)
	return nil
}

// node decodes the top-level schema itself, without its $defs.
func (t topSchema) node() (*node, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t.raw, &m); err != nil {
		return nil, err
	}
	delete(m, "$defs")
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	n := new(node)
	return n, json.Unmarshal(b, n)
}

type generator struct {
	doc     document
	decl    map[string]string          // Go type name -> declaration
	origin  map[string]string          // Go type name -> $ref it was generated from
	defs    map[string]*node           // def name -> decoded node, for defs emitted via ref
	results map[string]json.RawMessage // ResponseResult variant "type" const -> raw variant
}

// GenerateFile reads the schema at schemaPath, takes the herdr version from
// its file name (herdr-<version>.json) and generates the [Herdrcron]
// selection into package pkg.
func GenerateFile(schemaPath, pkg string) ([]byte, error) {
	version, err := VersionFromSchemaPath(schemaPath)
	if err != nil {
		return nil, err
	}
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, err
	}
	return Generate(schema, Options{Package: pkg, HerdrVersion: version, Selection: Herdrcron})
}

// Generate turns a herdr API schema document into gofmt'd Go source.
func Generate(schema []byte, opts Options) ([]byte, error) {
	g := &generator{decl: map[string]string{}, origin: map[string]string{}, defs: map[string]*node{}}
	if err := json.Unmarshal(schema, &g.doc); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	for _, name := range opts.Selection.Defs {
		if _, _, err := g.ref("#/schemas/success_response/$defs/" + name); err != nil {
			return nil, err
		}
	}

	if len(opts.Selection.Results) > 0 {
		if err := g.parseResults(); err != nil {
			return nil, err
		}
	}
	for _, r := range opts.Selection.Results {
		if err := g.result(r); err != nil {
			return nil, err
		}
	}

	if opts.Selection.Envelopes {
		if err := g.envelopes(); err != nil {
			return nil, err
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by herdr-schemagen; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Source: herdr %s API schema (protocol %d, schema_version %d).\n\n",
		opts.HerdrVersion, g.doc.Protocol, g.doc.SchemaVersion)
	fmt.Fprintf(&b, "package %s\n", opts.Package)
	for _, n := range slices.Sorted(maps.Keys(g.decl)) {
		b.WriteString("\n" + g.decl[n])
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt generated code: %w\n%s", err, b.Bytes())
	}
	return out, nil
}

// ref emits the def a $ref points at (once) and returns its Go name and node.
func (g *generator) ref(ref string) (string, *node, error) {
	rest, ok := strings.CutPrefix(ref, "#/schemas/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[1] != "$defs" {
		return "", nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	name := parts[2]
	if prev, seen := g.origin[name]; seen {
		if prev != ref {
			return "", nil, fmt.Errorf("type name %s used by both %q and %q", name, prev, ref)
		}
		return name, g.defs[name], nil
	}
	raw, ok := g.doc.Schemas[parts[0]].Defs[name]
	if !ok {
		return "", nil, fmt.Errorf("$ref %q: no such def", ref)
	}
	n := new(node)
	if err := json.Unmarshal(raw, n); err != nil {
		return "", nil, fmt.Errorf("def %s: %w", name, err)
	}
	g.origin[name] = ref
	g.defs[name] = n
	return name, n, g.named(name, n)
}

// envelopes emits Response[T] and ErrorResponse.
func (g *generator) envelopes() error {
	success, err := g.doc.Schemas["success_response"].node()
	if err != nil {
		return fmt.Errorf("success_response: %w", err)
	}
	id, result := success.Properties["id"], success.Properties["result"]
	req := slices.Sorted(slices.Values(success.Required))
	if len(success.Properties) != 2 || id == nil || result == nil || fmt.Sprint(id.Type) != "[string]" || fmt.Sprint(req) != "[id result]" {
		return fmt.Errorf("success_response: expected exactly required {id: string, result}, the shape Response[T] assumes")
	}
	g.origin["Response"] = "success_response"
	g.decl["Response"] = "// Response is a success response; T is one of the *Result types.\n" +
		"type Response[T any] struct {\n\tID string `json:\"id\"`\n\tResult T `json:\"result\"`\n}\n"

	errSchema, found := g.doc.Schemas["error_response"]
	if !found || errSchema.Title == "" {
		return fmt.Errorf("schema has no titled error_response")
	}
	n, err := errSchema.node()
	if err != nil {
		return fmt.Errorf("error_response: %w", err)
	}
	g.origin[errSchema.Title] = "error_response"
	return g.named(errSchema.Title, n)
}

// parseResults indexes success_response's ResponseResult oneOf variants by
// their "type" const.
func (g *generator) parseResults() error {
	var responseResult struct {
		OneOf []json.RawMessage `json:"oneOf"`
	}
	if err := json.Unmarshal(g.doc.Schemas["success_response"].Defs["ResponseResult"], &responseResult); err != nil {
		return fmt.Errorf("ResponseResult: %w", err)
	}
	g.results = map[string]json.RawMessage{}
	for i, raw := range responseResult.OneOf {
		var probe struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("ResponseResult variant %d: %w", i, err)
		}
		if _, dup := g.results[probe.Properties.Type.Const]; dup {
			return fmt.Errorf("ResponseResult has two variants with type %q", probe.Properties.Type.Const)
		}
		g.results[probe.Properties.Type.Const] = raw
	}
	return nil
}

// result emits one ResponseResult variant as a named struct.
func (g *generator) result(r Result) error {
	name := r.Name
	if name == "" {
		name = camel(r.Type) + "Result"
	}
	raw, found := g.results[r.Type]
	if !found {
		return fmt.Errorf("ResponseResult has no variant with type %q", r.Type)
	}
	origin := "ResponseResult variant " + r.Type
	if prev, seen := g.origin[name]; seen {
		return fmt.Errorf("type name %s used by both %q and %q; set Result.Name", name, prev, origin)
	}
	g.origin[name] = origin
	n := new(node)
	if err := json.Unmarshal(raw, n); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return g.named(name, n)
}

func (g *generator) named(name string, n *node) error {
	switch {
	case len(n.Enum) > 0:
		return g.enum(name, n)
	case !n.isObject():
		return fmt.Errorf("%s: only object and string-enum defs are understood, got type %v", name, n.Type)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "type %s struct {\n", name)
	required := map[string]bool{}
	for _, r := range n.Required {
		required[r] = true
	}
	for _, p := range slices.Sorted(maps.Keys(n.Properties)) {
		goType, err := g.goType(name+"."+p, n.Properties[p], required[p])
		if err != nil {
			return err
		}
		tag := p
		if !required[p] {
			tag += ",omitempty"
		}
		fmt.Fprintf(&b, "\t%s %s `json:%q`\n", camel(p), goType, tag)
	}
	b.WriteString("}\n")
	g.decl[name] = b.String()
	return nil
}

func (g *generator) goType(path string, n *node, required bool) (string, error) {
	if n.Ref != "" {
		name, target, err := g.ref(n.Ref)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if !required && target.isObject() {
			return "*" + name, nil
		}
		return name, nil
	}
	if len(n.AnyOf) > 0 {
		return g.nullableRef(path, n)
	}
	types, nullable := n.Type.withoutNull()
	if len(types) != 1 {
		return "", fmt.Errorf("%s: unsupported type %v", path, n.Type)
	}
	var t string
	switch types[0] {
	case "array":
		if n.Items == nil {
			return "", fmt.Errorf("%s: array without items", path)
		}
		elem, err := g.goType(path+"[]", n.Items, true)
		if err != nil {
			return "", err
		}
		return "[]" + elem, nil // nil slice already encodes null
	case "object":
		if n.AdditionalProperties == nil || len(n.Properties) > 0 {
			return "", fmt.Errorf("%s: inline objects are only understood as maps (additionalProperties without properties)", path)
		}
		elem, err := g.goType(path+"{}", n.AdditionalProperties, true)
		if err != nil {
			return "", err
		}
		return "map[string]" + elem, nil // nil map already encodes null
	case "string":
		if n.Const != nil {
			if _, ok := n.Const.(string); !ok {
				return "", fmt.Errorf("%s: non-string const %v", path, n.Const)
			}
		}
		t = "string"
	case "boolean":
		t = "bool"
	case "integer":
		var ok bool
		if t, ok = integerFormats[n.Format]; !ok {
			return "", fmt.Errorf("%s: unsupported integer format %q", path, n.Format)
		}
	default:
		return "", fmt.Errorf("%s: unsupported type %q", path, types[0])
	}
	if nullable {
		t = "*" + t
	}
	return t, nil
}

func (g *generator) enum(name string, n *node) error {
	if len(n.Type) != 1 || n.Type[0] != "string" {
		return fmt.Errorf("%s: only string enums are understood, got type %v", name, n.Type)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "type %s string\n\nconst (\n", name)
	for _, v := range n.Enum {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: non-string enum value %v", name, v)
		}
		fmt.Fprintf(&b, "\t%s%s %s = %q\n", name, camel(s), name, s)
	}
	b.WriteString(")\n")
	g.decl[name] = b.String()
	return nil
}

// nullableRef handles anyOf [{$ref X}, {type: null}], which becomes *X.
func (g *generator) nullableRef(path string, n *node) (string, error) {
	if len(n.AnyOf) == 2 && n.AnyOf[0].Ref != "" && len(n.AnyOf[1].Type) == 1 && n.AnyOf[1].Type[0] == "null" {
		name, _, err := g.ref(n.AnyOf[0].Ref)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return "*" + name, nil
	}
	return "", fmt.Errorf("%s: unsupported anyOf (only [{$ref}, {type: null}] is understood)", path)
}

func (n *node) isObject() bool { return len(n.Type) == 1 && n.Type[0] == "object" }

var integerFormats = map[string]string{
	"":       "int64",
	"int32":  "int32",
	"int64":  "int64",
	"uint":   "uint",
	"uint32": "uint32",
	"uint64": "uint64",
}

// withoutNull splits "null" off a type list.
func (t typeList) withoutNull() (types []string, nullable bool) {
	for _, s := range t {
		if s == "null" {
			nullable = true
			continue
		}
		types = append(types, s)
	}
	return types, nullable
}

var initialisms = map[string]string{"id": "ID", "pid": "PID", "tty": "TTY", "url": "URL", "uri": "URI", "api": "API", "json": "JSON", "http": "HTTP", "ui": "UI"}

// camel turns snake_case into an exported Go identifier.
func camel(s string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' }) {
		if up, ok := initialisms[w]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}

// VersionFromSchemaPath reads the herdr version out of a schema file named
// herdr-<version>.json, the name `herdr api schema --json` output is saved
// under.
func VersionFromSchemaPath(path string) (string, error) {
	base := filepath.Base(path)
	v, ok := strings.CutPrefix(strings.TrimSuffix(base, ".json"), "herdr-")
	if !ok || v == "" || !strings.HasSuffix(base, ".json") {
		return "", fmt.Errorf("schema file %q is not named herdr-<version>.json", base)
	}
	return v, nil
}
