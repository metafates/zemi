// args.go ships the builtin tools' typed arguments: a run function takes its
// args struct, and ArgsOf generates the schema the model sees from the same
// struct, so every field is declared once.

package tools

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// schemaEnums is the enum a named arg type shows in the schema: the value is
// the var the run code validates against, so the words are written once.
var schemaEnums = map[reflect.Type]any{
	reflect.TypeFor[store.TodoStatus](): store.TodoStatuses,
	reflect.TypeFor[QuestionType]():     questionTypes,
	reflect.TypeFor[store.Scope]():      []store.Scope{store.ScopeProject, store.ScopeUser},
	reflect.TypeFor[lspAction]():        lspActions,
	reflect.TypeFor[lspDirection]():     []string{"incoming", "outgoing"},
}

// ArgsOf returns an args struct's properties and required list, the shape
// fantasy expects in ToolInfo.Parameters: the json tag names the field,
// omitempty leaves it optional and a desc tag describes it. Required is
// never nil, which strict providers refuse.
func ArgsOf(args any) (props map[string]any, required []string) {
	t := reflect.TypeOf(args)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return argsOf(t)
}

func argsOf(t reflect.Type) (props map[string]any, required []string) {
	props, required = map[string]any{}, []string{}
	for f := range t.Fields() {
		name := f.Tag.Get("json")
		if name == "" || name == "-" {
			continue
		}
		name, _, _ = strings.Cut(name, ",")
		prop := argSchema(f.Type)
		if d := f.Tag.Get("desc"); d != "" {
			prop["description"] = d
		}
		if e, ok := schemaEnums[f.Type]; ok {
			prop["enum"] = e
		}
		props[name] = prop
		if !strings.Contains(f.Tag.Get("json"), "omitempty") {
			required = append(required, name)
		}
	}
	return props, required
}

// argSchema is one property's schema. A nested struct recurses as an object
// whose required list is only sent when it has entries, like the hand maps.
func argSchema(t reflect.Type) map[string]any {
	prop := map[string]any{}
	switch t.Kind() {
	case reflect.Pointer:
		return argSchema(t.Elem())
	case reflect.String:
		prop["type"] = "string"
	case reflect.Bool:
		prop["type"] = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		prop["type"] = "integer"
	case reflect.Float32, reflect.Float64:
		prop["type"] = "number"
	case reflect.Slice, reflect.Array:
		prop["type"] = "array"
		prop["items"] = argSchema(t.Elem())
	case reflect.Struct:
		iprops, ireq := argsOf(t)
		prop["type"] = "object"
		prop["properties"] = iprops
		if len(ireq) > 0 {
			prop["required"] = ireq
		}
	default:
		// a kind an args field never holds shows no type
	}
	return prop
}

// remarshal decodes a validated input map into an args struct: the values
// are the ones ParseInput's type checks and unquote coercion wrote.
func remarshal(in map[string]any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// typed adapts a run function that takes its args struct to the map the
// pipeline carries: the map ParseInput validated and coerced re-marshals
// into the struct, so a sloppy provider's quoted "60000" or "true" keeps
// working.
func typed[T any](fn func(context.Context, *Pipeline, T) (fantasy.ToolResponse, error)) func(context.Context, *Pipeline, map[string]any) (fantasy.ToolResponse, error) {
	return func(ctx context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
		var args T
		if err := remarshal(in, &args); err != nil {
			return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
		}
		return fn(ctx, p, args)
	}
}
