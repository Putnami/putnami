package config

import (
	"reflect"
	"slices"
	"strings"
)

// SensitiveFields returns the JSON field names of every field on T whose
// struct tag declares `sensitive:"true"`. Use this to drive log redaction,
// serializer output filtering, or any other runtime decision that needs to
// distinguish secret values from plaintext config.
//
// Pointer types are dereferenced, so Config[*DatabaseOptions](...) and
// Config[DatabaseOptions](...) return the same fields. Anonymous (embedded)
// struct fields are traversed, with sensitive subfields reported under
// their own JSON names. Returns nil if (after dereferencing) T is not a
// struct.
//
//	type DatabaseOptions struct {
//	    Host     string `json:"host"`
//	    Password string `json:"password" sensitive:"true"`
//	}
//
//	var DatabaseConfig = config.Config[DatabaseOptions]("database")
//	config.SensitiveFields(DatabaseConfig) // -> []string{"password"}
func SensitiveFields[T any](def Definition[T]) []string {
	_ = def
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return sensitiveFieldNames(t)
}

func sensitiveFieldNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous {
			ft := field.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, sensitiveFieldNames(ft)...)
				continue
			}
		}
		if field.Tag.Get("sensitive") != "true" {
			continue
		}
		name := field.Name
		if jsonTag := field.Tag.Get("json"); jsonTag != "" {
			n, _, _ := strings.Cut(jsonTag, ",")
			if n != "" && n != "-" {
				name = n
			}
		}
		out = append(out, name)
	}
	return out
}

// IsSensitiveField reports whether the named field on T is marked
// sensitive in its struct tag. The name is matched against the field's
// JSON name (or its Go name if no json tag is set).
//
// Returns false if T is not a struct or the field does not exist.
func IsSensitiveField[T any](def Definition[T], name string) bool {
	return slices.Contains(SensitiveFields(def), name)
}
