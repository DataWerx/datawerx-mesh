package contract

import (
	"encoding/json"
	"reflect"
	"testing"
)

// phase is an enum that renders as a string via TextMarshaler even though its
// kind is int — the case the generator must special-case.
type phase int

func (phase) MarshalText() ([]byte, error) { return []byte("x"), nil }

type inner struct {
	A string `json:"a"`
	B int    `json:"b,omitempty"`
}

type sample struct {
	Name     string            `json:"name"`
	Count    int64             `json:"count,omitempty"`
	Ratio    float64           `json:"ratio"`
	On       bool              `json:"on,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Children []inner           `json:"children,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Opt      *string           `json:"opt,omitempty"`
	Phase    phase             `json:"phase"`
	Skipped  string            `json:"-"`
	Nested   inner             `json:"nested"`
}

func TestSchema_EnvelopeAndTitle(t *testing.T) {
	s := Schema(sample{}, "Sample")
	if s["$schema"] != Dialect {
		t.Errorf("missing dialect: %v", s["$schema"])
	}
	if s["title"] != "Sample" {
		t.Errorf("missing title: %v", s["title"])
	}
	if s["type"] != "object" {
		t.Errorf("root should be object: %v", s["type"])
	}
}

func TestSchema_FieldTypes(t *testing.T) {
	props := Schema(sample{}, "")["properties"].(map[string]any)

	want := map[string]string{
		"name":  "string",
		"count": "integer",
		"ratio": "number",
		"on":    "boolean",
		"phase": "string", // TextMarshaler enum
	}
	for field, typ := range want {
		got := props[field].(map[string]any)["type"]
		if got != typ {
			t.Errorf("%s: want type %q, got %q", field, typ, got)
		}
	}

	if _, ok := props["Skipped"]; ok {
		t.Error("json:\"-\" field must not appear")
	}

	// Slice of struct → array of object.
	children := props["children"].(map[string]any)
	if children["type"] != "array" {
		t.Errorf("children should be array: %v", children)
	}
	items := children["items"].(map[string]any)
	if items["type"] != "object" {
		t.Errorf("children items should be object: %v", items)
	}

	// Map → object with additionalProperties.
	labels := props["labels"].(map[string]any)
	if labels["type"] != "object" || labels["additionalProperties"] == nil {
		t.Errorf("labels should be an object with additionalProperties: %v", labels)
	}

	// Pointer is unwrapped to its base type.
	if props["opt"].(map[string]any)["type"] != "string" {
		t.Errorf("opt should unwrap to string: %v", props["opt"])
	}
}

func TestSchema_RequiredExcludesOmitempty(t *testing.T) {
	req := Schema(sample{}, "")["required"].([]string)
	got := map[string]bool{}
	for _, r := range req {
		got[r] = true
	}
	// Required: the fields without omitempty.
	for _, r := range []string{"name", "ratio", "phase", "nested"} {
		if !got[r] {
			t.Errorf("expected %q to be required", r)
		}
	}
	// Not required: the omitempty fields.
	for _, r := range []string{"count", "on", "tags", "children", "labels", "opt"} {
		if got[r] {
			t.Errorf("%q has omitempty and must not be required", r)
		}
	}
	// required must be sorted for stable output.
	if !sortedStrings(req) {
		t.Errorf("required not sorted: %v", req)
	}
}

func TestSchema_NestedStructRequired(t *testing.T) {
	props := Schema(sample{}, "")["properties"].(map[string]any)
	nested := props["nested"].(map[string]any)
	req, _ := nested["required"].([]string)
	if len(req) != 1 || req[0] != "a" {
		t.Errorf("nested required should be [a] (b is omitempty): %v", req)
	}
}

func TestJSON_IsValidAndStable(t *testing.T) {
	a, err := JSON(sample{}, "Sample")
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var into map[string]any
	if err := json.Unmarshal(a, &into); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	b, _ := JSON(sample{}, "Sample")
	if string(a) != string(b) {
		t.Error("schema JSON is not deterministic")
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func TestSchema_EmbeddedInlined(t *testing.T) {
	type withEmbed struct {
		inner        // embedded, inlined
		C     string `json:"c"`
	}
	props := Schema(withEmbed{}, "")["properties"].(map[string]any)
	for _, f := range []string{"a", "b", "c"} {
		if _, ok := props[f]; !ok {
			t.Errorf("embedded field %q should be inlined into the parent", f)
		}
	}
}

func TestSchema_TopLevelKinds(t *testing.T) {
	cases := []struct {
		v    any
		kind string
	}{
		{"", "string"},
		{int32(0), "integer"},
		{3.14, "number"},
		{true, "boolean"},
		{[]int{}, "array"},
		{map[string]int{}, "object"},
	}
	for _, c := range cases {
		if got := schemaFor(reflect.TypeOf(c.v))["type"]; got != c.kind {
			t.Errorf("%T: want %q, got %q", c.v, c.kind, got)
		}
	}
}
