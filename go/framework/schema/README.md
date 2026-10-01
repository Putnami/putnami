# Schema Validation

The `schema` package provides struct-tag-based validation for Go structs. It supports required/optional fields, type coercion, composable constraints, and default values.

## Validating Data

Validate a map of values against a struct type:

```go
import (
    "reflect"
    "go.putnami.dev/schema"
)

type UserInput struct {
    Name  string `json:"name" validate:"required,minlen=2,maxlen=50"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age" validate:"min=0,max=150"`
}

result := schema.Validate(reflect.TypeOf(UserInput{}), map[string]any{
    "name":  "Alice",
    "email": "alice@example.com",
    "age":   30,
})

if result.HasErrors() {
    for _, err := range result.Errors {
        fmt.Printf("%s: %s\n", err.Field, err.Message)
    }
}
// result.Data contains validated values
```

For an already-decoded JSON body, use `ValidateDecoded` with the typed value and
the original JSON bytes. It keeps wire presence separate from Go zero values,
validates nested structs, slices, and maps recursively, and writes optional
defaults into the decoded value. On this path, a required `false`, `0`, or empty
string is valid when the property was present; an absent property and an
explicit `null` remain distinct.

`JSONFields` exposes the same field promotion and dominance rules used by
`encoding/json` for generators. `DefaultJSON` converts a `default` tag to JSON
for its declared Go type and rejects invalid defaults without echoing their
authored value in the error. The lower-level `Validate` map API keeps its
historical top-level field selection.

## Validate Tags

Multiple constraints are comma-separated: `validate:"required,minlen=3,maxlen=100"`

| Tag | Description |
|-----|-------------|
| `required` | Field must be present and non-zero |
| `uuid` | UUID v4 format |
| `email` | Email format |
| `url` | Valid `http`/`https` URL (rejects `javascript:`/`data:`/`file:`/other schemes) |
| `min=N` | Minimum numeric value |
| `max=N` | Maximum numeric value |
| `minlen=N` | Minimum string length |
| `maxlen=N` | Maximum string length |
| `pattern=REGEX` | Regex match |
| `oneof=a\|b\|c` | Must be one of the listed values |

## Default Values

Use the `default` struct tag for fallback values:

```go
type Options struct {
    Label   string `json:"label"`
    Count   int    `json:"count" default:"10"`
    Verbose bool   `json:"verbose" default:"true"`
}

result := schema.Validate(reflect.TypeOf(Options{}), map[string]any{})
// result.Data["count"] == 10
// result.Data["verbose"] == true
```

## Type Coercion

Enable automatic type coercion (e.g., string `"25"` to int `25`):

```go
result := schema.Validate(reflect.TypeOf(UserInput{}), data,
    schema.WithCoerce(),
)
```

## Error Labels

Prefix error field names for nested validation contexts:

```go
result := schema.Validate(reflect.TypeOf(UserInput{}), data,
    schema.WithLabel("body"),
)
// Errors report "body.name" instead of "name"
```

## Validation Result

```go
type Result struct {
    Data   map[string]any  // Validated and coerced values
    Errors []FieldError    // Validation errors
}

type FieldError struct {
    Field      string  // Field name (with optional prefix)
    Message    string  // Human-readable error message
    Constraint string  // Constraint that failed (e.g., "required", "minlen")
}
```

## Declaration faults

An unknown constraint name or an uncompilable pattern is reported as a field error
in its own right, before any value is looked at and whether or not a value was
supplied. A typo such as `validate:"requred"` therefore fails loudly instead of
silently making the field optional.

`Validate` never panics on a caller-supplied type: a nil or non-struct type returns
a structured schema error, and `*T` validates exactly like `T`.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/schema` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

The [struct validation specification](specs/struct-validation.json) defines the
contract, backed by [a broken constraint declaration is a fault, not a passing
field](doc/adr/0001-declaration-faults-are-not-validation-results.md).

Regression evidence covers [constraints, defaults, coercion, and
labels](schema_test.go), [the exported API contract](contract_test.go), [the
supplementary API contract](api_contract_test.go), and [rune counting, scheme
allowlists, and other edge cases](edge_cases_test.go).
