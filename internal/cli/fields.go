package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Settings on a command line are named by their path through the settings
// file — "proxy_port", "targets.all_entity_hooks" — because that file is
// what the user already reads, and a name that matches it needs no second
// vocabulary to learn.
//
// The walk is over the Go type's JSON tags rather than over a marshalled
// map, so a field that is absent from the file (omitempty, and empty)
// still has a name, a type and a value of its own. That is the difference
// between "this setting is at its default" and "there is no such setting".

// Field is one setting: where it is, what it holds, and what it is.
type Field struct {
	// Path is the dotted name, as the user types it.
	Path string `json:"path"`
	// Value is the value as JSON, which is how it is written back.
	Value string `json:"value"`
	// Kind is the shape: string, number, boolean, list, object.
	Kind string `json:"kind"`
	// Leaf is false for a field holding other fields; a list or a map is a
	// leaf, because it is set whole.
	Leaf bool `json:"leaf"`
}

// Fields walks a settings struct and returns every field in it, in
// declaration order, with the values v currently holds. A nil pointer is
// walked as its type, so an override nobody has set still has a name.
func Fields(v any) []Field {
	var out []Field
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil
	}
	walkFields(rv.Type(), rv, "", &out)
	return out
}

// walkFields walks a struct type, with v holding its values when there are
// any. An invalid v means this part of the settings is not set at all — an
// override nobody has given — and everything under it reads as null rather
// than as the zero values it would take, because "unset" and "set to zero"
// are different answers.
func walkFields(t reflect.Type, v reflect.Value, prefix string, out *[]Field) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
		if v.IsValid() && !v.IsNil() {
			v = v.Elem()
		} else {
			v = reflect.Value{}
		}
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := jsonName(f)
		if !ok {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		var fv reflect.Value
		if v.IsValid() {
			fv = v.Field(i)
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		// A struct (or a pointer to one) holds more settings; everything
		// else is set whole, lists and maps included.
		if ft.Kind() == reflect.Struct && !isTime(ft) {
			*out = append(*out, Field{Path: path, Value: encode(fv), Kind: "object"})
			walkFields(f.Type, fv, path, out)
			continue
		}
		*out = append(*out, Field{Path: path, Value: encode(fv), Kind: kindOf(ft), Leaf: true})
	}
}

func isTime(t reflect.Type) bool {
	return t.PkgPath() == "time" && t.Name() == "Time"
}

func kindOf(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map:
		return "object"
	}
	return t.Kind().String()
}

func encode(v reflect.Value) string {
	if !v.IsValid() {
		return "null"
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return "null"
	}
	return string(b)
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, true
}

// FieldAt returns the field at a dotted path.
func FieldAt(v any, path string) (Field, bool) {
	for _, f := range Fields(v) {
		if strings.EqualFold(f.Path, path) {
			return f, true
		}
	}
	return Field{}, false
}

// SetField writes value at a dotted path in the settings struct ptr points
// at. The value is read as JSON when it parses as JSON and as a plain
// string otherwise, so --value 8080, --value true and --value dark all mean
// what they look like.
//
// It is applied by unmarshalling a one-field patch onto the struct, which
// is how only the named setting changes and everything beside it is left
// exactly as it was.
func SetField(ptr any, path, value string) error {
	f, ok := FieldAt(ptr, path)
	if !ok {
		return fmt.Errorf("there is no setting named %q", path)
	}
	if !f.Leaf {
		return fmt.Errorf("%q holds other settings; set one of them (\"settings ls\" lists them)", path)
	}
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		parsed = value // a bare word is the string it looks like
	}
	patch := parsed
	parts := strings.Split(f.Path, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		patch = map[string]any{parts[i]: patch}
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, ptr); err != nil {
		return fmt.Errorf("%s takes a %s: %w", path, f.Kind, err)
	}
	return nil
}
