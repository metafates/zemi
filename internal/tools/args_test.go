package tools

import (
	"reflect"
	"testing"

	"charm.land/fantasy"
)

type argsNested struct {
	City string `json:"city" desc:"where"`
}

type argsTest struct {
	Name    string       `json:"name" desc:"the name"`
	Count   int          `json:"count,omitempty"`
	Kind    QuestionType `json:"kind,omitempty"`
	Disable bool         `json:"disable,omitempty"`
	Tags    []string     `json:"tags,omitempty"`
	Nested  *argsNested  `json:"nested,omitempty"`
}

// TestArgsOf pins the schema one args struct generates and the round trip a
// typed run function rides: ParseInput coerces a sloppy provider's quoted
// scalars and remarshal lands them in the struct.
func TestArgsOf(t *testing.T) {
	props, required := ArgsOf(argsTest{})
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"description", at(t, props, "name", "description"), "the name"},
		{"type", at(t, props, "name", "type"), "string"},
		{"required", required, []string{"name"}},
		{"enum", at(t, props, "kind", "enum"), _questionTypes},
		{"slice", at(t, props, "tags", "items", "type"), "string"},
		{"nested city", at(t, props, "nested", "properties", "city", "description"), "where"},
		{"nested required", at(t, props, "nested", "required"), []string{"city"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	info := fantasy.ToolInfo{Name: "test", Parameters: props, Required: required}
	m, err := ParseInput(info, `{"name":"x","count":"60000","disable":"true","nested":{"city":"y"},"tags":["a"]}`, false)
	if err != nil {
		t.Fatal(err)
	}
	var args argsTest
	if err := remarshal(m, &args); err != nil {
		t.Fatal(err)
	}
	if args.Count != 60000 || !args.Disable || args.Nested.City != "y" || len(args.Tags) != 1 {
		t.Fatalf("args = %+v", args)
	}
}

// at walks the object keys of v, failing the test where one is missing.
func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %T is not an object", k, v)
		}
		v = obj[k]
	}
	return v
}
