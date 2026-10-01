package http

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// SecurityRule represents a security authorization rule. Implemented by security.Options
// and security.Guard. Defined here so api.EndpointBuilder.Secure can accept any rule
// without forcing api/ to import the security package.
type SecurityRule interface {
	// Middleware returns the authorization middleware for this rule.
	Middleware() Middleware
}

// SecurityClaims is optionally implemented by SecurityRule values that declare
// concrete role and scope requirements (e.g. security.Options). Documentation
// generators such as openapi use it to read declared claims through a typed,
// rename-safe accessor instead of reflecting on field names. Rules that do not
// implement it (guard functions, custom rules) remain documented as auth-gated,
// just without specific claims.
type SecurityClaims interface {
	// SecurityRoles returns the roles this rule requires.
	SecurityRoles() []string
	// SecurityScopes returns the scopes this rule requires.
	SecurityScopes() []string
}

// SecurityOptionalAuthentication is optionally implemented by SecurityRule
// values whose middleware serves a request that presents no credential and
// applies the rule to an authenticated caller only: a public read that answers
// more to an authorized caller (security.Options with Optional does). Contract
// generators read it to publish an anonymous alternative after the credentialed
// ones. A rule that does not implement it, or reports false, requires
// authentication, and its contract never offers an anonymous alternative.
type SecurityOptionalAuthentication interface {
	// OptionalAuthentication reports whether the rule serves a request that
	// presents no credential.
	OptionalAuthentication() bool
}

// SecurityAuthorization is the transport-neutral declarative authorization
// policy exposed by a SecurityRule. Slice fields retain AND (`*All`) and OR
// (`*Any`) semantics rather than flattening them into prose.
type SecurityAuthorization struct {
	Clients      []string
	ScopesAll    []string
	ScopesAny    []string
	RolesAll     []string
	RolesAny     []string
	ExcludePaths []string
}

// SecurityAuthorizationPolicy is implemented by declarative security rules.
// Executable guards intentionally do not implement it, allowing first-party
// contract generation to fail when their semantics cannot be represented.
type SecurityAuthorizationPolicy interface {
	SecurityAuthorizationPolicy() SecurityAuthorization
}

// EndpointContext extends Context with validated input and resolved DI dependencies.
// Populated by the api builder pipeline before the handler runs.
type EndpointContext struct {
	*Context
	// ValidatedParams contains validated path parameters.
	ValidatedParams map[string]any
	// ValidatedQuery contains validated query parameters.
	ValidatedQuery map[string]any
	// ValidatedBody contains the validated request body.
	ValidatedBody map[string]any
	// DecodedBody is the exact typed first-party body decoded directly from the
	// request bytes. It preserves integer width and closed-object semantics.
	DecodedBody any
	// Injected contains resolved DI dependencies.
	Injected map[string]any
}

// ParamsAs converts validated path parameters to a typed struct.
func ParamsAs[T any](ctx *EndpointContext) (T, error) {
	return mapAs[T](ctx.ValidatedParams)
}

// QueryAs converts validated query parameters to a typed struct.
func QueryAs[T any](ctx *EndpointContext) (T, error) {
	return mapAs[T](ctx.ValidatedQuery)
}

// BodyAs converts the validated request body to a typed struct.
func BodyAs[T any](ctx *EndpointContext) (T, error) {
	if ctx != nil && ctx.DecodedBody != nil {
		if value, ok := ctx.DecodedBody.(T); ok {
			return value, nil
		}
	}
	return mapAs[T](ctx.ValidatedBody)
}

// InjectedAs retrieves a named DI dependency with type safety.
func InjectedAs[T any](ctx *EndpointContext, name string) (T, error) {
	val, ok := ctx.Injected[name]
	if !ok {
		var zero T
		return zero, fmt.Errorf("injected dependency %q not found", name)
	}
	typed, ok := val.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("injected dependency %q: expected %T, got %T", name, zero, val)
	}
	return typed, nil
}

// mapAs converts a map[string]any to a typed struct using reflection. Struct fields are
// matched by their json tag name (or lowercased field name).
func mapAs[T any](data map[string]any) (T, error) {
	var result T
	if data == nil {
		return result, nil
	}

	rv := reflect.ValueOf(&result).Elem()
	rt := rv.Type()

	if rt.Kind() == reflect.Map {
		mapVal := reflect.MakeMapWithSize(rt, len(data))
		for k, v := range data {
			mapVal.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(v))
		}
		rv.Set(mapVal)
		return result, nil
	}

	if rt.Kind() != reflect.Struct {
		// Non-struct, non-map types: round-trip through JSON.
		b, err := json.Marshal(data)
		if err != nil {
			return result, fmt.Errorf("marshal validated data: %w", err)
		}
		if err := json.Unmarshal(b, &result); err != nil {
			return result, fmt.Errorf("unmarshal validated data: %w", err)
		}
		return result, nil
	}

	for i := range rt.NumField() {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Tag.Get("json")
		if comma := indexOf(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" || name == "-" {
			name = lowercaseFirst(field.Name)
		}
		val, ok := data[name]
		if !ok {
			continue
		}
		fieldVal := rv.Field(i)
		if val == nil {
			continue
		}
		src := reflect.ValueOf(val)
		if src.Type().AssignableTo(fieldVal.Type()) {
			fieldVal.Set(src)
		} else if src.Type().ConvertibleTo(fieldVal.Type()) {
			fieldVal.Set(src.Convert(fieldVal.Type()))
		}
	}
	return result, nil
}

func indexOf(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lowercaseFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'A' && s[0] <= 'Z' {
		return string(s[0]+32) + s[1:]
	}
	return s
}
