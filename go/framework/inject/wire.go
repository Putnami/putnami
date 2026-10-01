package inject

import (
	"reflect"
	"strconv"
	"strings"

	"go.putnami.dev/errors"
)

// CodeWireIncomplete is returned by WireStrict when one or more wireable
// fields could not be satisfied from the container.
const CodeWireIncomplete errors.Code = "inject.wire_incomplete"

// Wire resolves exported struct fields from the DI container, eliminating
// manual resolve boilerplate in plugin Configure methods.
//
// Wire scans the target struct for exported pointer and interface fields
// that are currently nil. For each such field, it looks up the type in the
// container and sets the field if a matching provider exists.
//
// Fields that are already set (non-nil) are never overwritten.
// Fields whose types are not registered in the container are silently skipped
// — Wire is the lenient, best-effort entry point and always returns a nil
// error. Use [WireStrict] when an unresolved field must surface as an error
// rather than a later nil-pointer panic.
// Use the struct tag `inject:"-"` to exclude a field from wiring.
//
// target must be a pointer to a struct. Non-struct targets are silently ignored.
//
// Example:
//
//	type MyPlugin struct {
//	    DB  *sql.Pool
//	    Log *logger.Logger
//	}
//
//	func (p *MyPlugin) Configure(ctx context.Context, owner *app.Module) error {
//	    // p.DB and p.Log are already set by the framework's auto-wire phase
//	    p.DB.Query(...)
//	}
func Wire(cc *ContainerContext, target any) error {
	wireFields(cc, target)
	return nil
}

// WireStrict behaves like [Wire] — it populates the same exported, currently
// nil pointer and interface fields from the container — but returns a non-nil
// error enumerating every wireable field it could not satisfy. This turns a
// misconfigured or misspelled dependency into a clear wiring error at
// Configure time instead of a deferred nil-pointer panic.
//
// A field is considered unsatisfied when its type is not registered in the
// container (or its provider fails to resolve). Fields excluded via
// `inject:"-"`, already-set fields, unexported fields, and non-pointer /
// non-interface fields are not candidates and never cause an error. A nil
// container or non-struct target satisfies trivially (nil error), matching
// [Wire].
//
// The returned error carries code [CodeWireIncomplete] and a "fields" attribute
// listing the unresolved field names, so callers can branch on it with
// errors.Is.
func WireStrict(cc *ContainerContext, target any) error {
	unresolved := wireFields(cc, target)
	if len(unresolved) == 0 {
		return nil
	}

	name := targetTypeName(target)
	return errors.New(CodeWireIncomplete,
		"could not wire "+strconv.Itoa(len(unresolved))+" field(s) on "+name+": "+strings.Join(unresolved, ", "),
		errors.String("type", name),
		errors.String("fields", strings.Join(unresolved, ", ")),
	)
}

// wireFields populates the eligible exported fields of target from cc and
// returns the names of the fields that could not be resolved. It is the shared
// core behind Wire (which ignores the result) and WireStrict (which reports
// it), so both stay byte-for-byte consistent about which fields are candidates.
func wireFields(cc *ContainerContext, target any) []string {
	if cc == nil {
		return nil
	}

	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return nil
	}

	v = v.Elem()
	t := v.Type()

	var unresolved []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		// Skip fields tagged with inject:"-"
		if tag, ok := field.Tag.Lookup("inject"); ok && tag == "-" {
			continue
		}

		fieldVal := v.Field(i)
		if !fieldVal.CanSet() {
			continue
		}

		// Only wire pointer and interface types
		kind := field.Type.Kind()
		if kind != reflect.Ptr && kind != reflect.Interface {
			continue
		}

		// Skip fields that are already set
		if !fieldVal.IsNil() {
			continue
		}

		// Try to resolve from container. An unresolved type is recorded (and
		// reported by WireStrict) but left nil so the lenient Wire keeps going.
		token := classToken{typ: field.Type}
		val, err := cc.Get(token)
		if err != nil {
			unresolved = append(unresolved, field.Name)
		} else {
			fieldVal.Set(reflect.ValueOf(val))
		}
	}

	return unresolved
}

// targetTypeName returns a human-readable name for the wired target, used in
// strict-mode error messages. It dereferences the pointer-to-struct so the
// message reads "MyPlugin" rather than "*pkg.MyPlugin".
func targetTypeName(target any) string {
	t := reflect.TypeOf(target)
	if t == nil {
		return "<nil>"
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.String()
}
