// Package contract derives a JSON Schema (draft 2020-12) from a Go type by
// reflection. It exists so the mesh's published data contracts — the snapshot
// (0005) and the dependency graph (0009) — have a machine-readable schema a
// consumer can validate against, generated from the very structs that produce
// the JSON so the schema can never drift from the wire format.
//
// It is deliberately small and dependency-free: it handles exactly the shapes
// the contracts use (structs, slices, string-keyed maps, pointers, the basic
// scalar kinds, and types that render as a string via encoding.TextMarshaler).
// It is not a general-purpose schema compiler.
package contract

import (
	"encoding"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Dialect is the JSON Schema dialect the generated schemas declare.
const Dialect = "https://json-schema.org/draft/2020-12/schema"

// textMarshaler is used to detect types that marshal to a JSON string even
// though their Go kind is not a string (e.g. an enum int with a MarshalText).
var textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()

// Schema returns the JSON Schema for the type of v as a map ready to marshal.
// title is set as the schema's title when non-empty.
func Schema(v any, title string) map[string]any {
	s := schemaFor(reflect.TypeOf(v))
	s["$schema"] = Dialect
	if title != "" {
		s["title"] = title
	}
	return s
}

// JSON returns the indented JSON Schema for the type of v.
func JSON(v any, title string) ([]byte, error) {
	return json.MarshalIndent(Schema(v, title), "", "  ")
}

// schemaFor builds the schema node for a single type.
func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// A type that marshals via TextMarshaler is a JSON string regardless of its
	// underlying kind (e.g. verify.Status renders as "PASS"/"WARN"/"FAIL").
	if t.Implements(textMarshalerType) || reflect.PtrTo(t).Implements(textMarshalerType) {
		return map[string]any{"type": "string"}
	}

	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	case reflect.Struct:
		return structSchema(t)
	default:
		// interface{} and anything unhandled: accept anything.
		return map[string]any{}
	}
}

// structSchema builds an object schema from a struct's JSON-tagged fields.
// Fields without ",omitempty" are required; embedded structs are inlined.
func structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported, non-embedded: not serialized
		}
		name, opts, ok := jsonField(f)
		if !ok {
			continue // json:"-"
		}
		if f.Anonymous && name == "" {
			// Embedded struct with no explicit json name: inline its properties.
			inlineStruct(f.Type, props, &required)
			continue
		}
		props[name] = schemaFor(f.Type)
		if !opts["omitempty"] {
			required = append(required, name)
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

// inlineStruct merges an embedded struct's properties into the parent.
func inlineStruct(t reflect.Type, props map[string]any, required *[]string) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	sub := structSchema(t)
	for k, v := range sub["properties"].(map[string]any) {
		props[k] = v
	}
	if req, ok := sub["required"].([]string); ok {
		*required = append(*required, req...)
	}
}

// jsonField returns the JSON name, the option set, and whether the field is
// serialized at all (false for json:"-").
func jsonField(f reflect.StructField) (name string, opts map[string]bool, ok bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", nil, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	opts = map[string]bool{}
	for _, o := range parts[1:] {
		opts[o] = true
	}
	if name == "" && !f.Anonymous {
		name = f.Name
	}
	return name, opts, true
}
