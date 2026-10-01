# go.putnami.dev/schema

Struct-tag-based validation with type coercion, constraints, defaults, and error labels.

## Quick Start

```go
import (
    "reflect"
    "go.putnami.dev/schema"
)

type CreateUser struct {
    Name  string `json:"name" validate:"required,minlen=2,maxlen=100"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age" validate:"min=0,max=150"`
    Role  string `json:"role" validate:"oneof=admin|user|guest" default:"user"`
}

// Validate a map against a struct type
values := map[string]any{"name": "Alice", "email": "alice@example.com", "age": 30}
result := schema.Validate(reflect.TypeOf(CreateUser{}), values)

if result.HasErrors() {
    for _, e := range result.Errors {
        fmt.Printf("%s: %s (%s)\n", e.Field, e.Message, e.Constraint)
    }
}

// result.Data contains validated values (with defaults applied)
```

Use `schema.ValidateDecoded(reflect.TypeOf(T{}), &decoded, rawJSON)` at a typed
JSON boundary. It validates required wire presence, explicit nulls, exact
numeric values, and nested structs/slices/maps, then applies optional defaults
to `decoded`. `schema.Validate` remains the lower-level map API with its
historical top-level field selection and a `required` rule that also rejects
zero values.

`schema.JSONFields` returns the deterministic `encoding/json` field selection
for a struct. `schema.DefaultJSON` converts one `default` tag into typed JSON and
returns a value-free error when the tag cannot represent the field type.

## Validation Tags

| Tag | Description | Example |
|-----|-------------|---------|
| `required` | Must be non-zero | `validate:"required"` |
| `email` | Valid email format | `validate:"email"` |
| `uuid` | Valid UUID v4 format | `validate:"uuid"` |
| `url` | Valid `http`/`https` URL (rejects `javascript:`/`data:`/`file:`/other schemes) | `validate:"url"` |
| `min=N` | Minimum numeric value | `validate:"min=1"` |
| `max=N` | Maximum numeric value | `validate:"max=100"` |
| `minlen=N` | Minimum string length | `validate:"minlen=2"` |
| `maxlen=N` | Maximum string length | `validate:"maxlen=255"` |
| `pattern=RE` | Regex pattern match | `validate:"pattern=^[a-z]+$"` |
| `oneof=a\|b\|c` | Must be one of values (pipe-separated) | `validate:"oneof=admin\|user"` |

Multiple constraints are comma-separated: `validate:"required,minlen=3,maxlen=100"`

## Options

```go
// Enable type coercion (string → int, float, bool)
result := schema.Validate(reflect.TypeOf(T{}), values, schema.WithCoerce())

// Prefix error field names (e.g., "body.name", "query.page")
result := schema.Validate(reflect.TypeOf(T{}), values, schema.WithLabel("body"))
```

## Result

```go
type Result struct {
    Data   map[string]any  // validated values with defaults applied
    Errors []FieldError    // validation errors
}

result.HasErrors() bool  // true if validation failed
```

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [struct
validation specification](specs/struct-validation.json) and the
[declaration-faults ADR](doc/adr/0001-declaration-faults-are-not-validation-results.md).
Before v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
