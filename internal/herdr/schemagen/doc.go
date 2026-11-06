// Package schemagen turns herdr's API JSON schema (`herdr api schema --json`)
// into Go types. It emits only a [Selection] of types plus everything they
// reference, and fails on any schema construct it does not understand rather
// than falling back to any.
//
// Mapping rules:
//
//   - object def → struct; fields sorted by JSON name; snake_case names become
//     CamelCase with Go initialisms (pane_id → PaneID).
//   - required property → no omitempty; optional property → omitempty.
//   - pointers mean "may be null": "type": [T, "null"] → *T, and
//     anyOf [{$ref X}, {type: null}] → *X. Optional non-nullable scalars stay
//     values (absent decodes as the zero value), except that an optional $ref
//     to an object is *X so absence is visible.
//   - nullable or optional arrays and maps are plain slices/maps (nil is null).
//   - string enum def → named string type with typed constants
//     (AgentStatus "idle" → AgentStatusIdle).
//   - integer formats: uint → uint, uint32 → uint32, uint64 → uint64,
//     int32 → int32, int64 or none → int64.
//   - additionalProperties: {type: string} without properties → map[string]string.
//   - "const" string (the type tag of result variants) → string.
//   - Validation-only keywords (minimum, maximum, default, pattern,
//     propertyNames, maxProperties, ...) are ignored; any other keyword,
//     inline object, non-null type union, or other anyOf is an error.
//
// ResponseResult variants are picked by their "type" const and named
// CamelCase(type)+"Result" unless [Result.Name] overrides it; name collisions
// are errors. With [Selection.Envelopes] it also emits the generic success
// envelope Response[T] and ErrorResponse from error_response.
package schemagen
