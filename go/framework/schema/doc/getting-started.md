# Schema Validation

`go.putnami.dev/schema` provides struct-tag-based validation for Go applications. It validates either `map[string]any` input or an already-decoded typed JSON value against struct definitions using the `validate` tag, with support for default values, type coercion, and composable constraints. The module uses only the Go standard library.

## Defining a Schema

A schema is a regular Go struct. Fields are matched by their `json` tag name (or struct field name if no `json` tag is present). Use the `validate` tag to declare constraints and the `default` tag to set fallback values.

```go
type CreateUserInput struct {
    Name  string `json:"name"  validate:"required,minlen=2,maxlen=50"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age"   validate:"min=0,max=150"`
}
```

For `ValidateDecoded` and `JSONFields`, field selection matches `encoding/json`:
exported fields are inspected, anonymous structs promote their selected
children (including exported children of an unexported anonymous struct),
shallower or explicitly tagged fields win name conflicts, ambiguous ties are
omitted, and `json:"-"` is skipped. Without a valid explicit tag name, the exact
Go field name is used. `Validate` retains its historical top-level map-key
selection so existing low-level callers keep the same behavior.

## Validating Data

Pass a `reflect.Type` and a `map[string]any` to `schema.Validate`. The function returns a `Result` containing validated data and any errors.

```go
import (
    "reflect"

    "go.putnami.dev/schema"
)

result := schema.Validate(reflect.TypeOf(CreateUserInput{}), map[string]any{
    "name":  "Alice",
    "email": "alice@example.com",
    "age":   30,
})

if result.HasErrors() {
    for _, err := range result.Errors {
        fmt.Printf("field %s: %s\n", err.Field, err.Message)
    }
    return
}

// Access validated data
name := result.Data["name"].(string)
```

### Validating a decoded JSON value

At a typed JSON boundary, pass both the decoded value and its original bytes to
`ValidateDecoded`:

```go
var input CreateUserInput
if err := json.Unmarshal(rawBody, &input); err != nil {
    return err
}
result := schema.ValidateDecoded(reflect.TypeOf(input), &input, rawBody,
    schema.WithLabel("body"),
)
```

The raw bytes preserve information a Go value alone cannot: whether a property
was absent, explicitly `null`, or present as `false`, `0`, `""`, `[]`, or `{}`.
Required means present on this path. Nested structs, slice items, and map values
are validated recursively, exact integer bounds are retained, and optional
defaults are written into `input` before it is used. `Validate` remains the
low-level map API and keeps its historical required-and-non-zero behavior.

## Supported Constraints

Multiple constraints are comma-separated in the `validate` tag.

| Constraint | Example | Description |
|---|---|---|
| `required` | `validate:"required"` | Field must be present and non-zero |
| `uuid` | `validate:"uuid"` | UUID v4 format |
| `email` | `validate:"email"` | Email address format |
| `url` | `validate:"url"` | Valid `http`/`https` URL (other schemes such as `javascript:`, `data:`, `file:` are rejected) |
| `min=N` | `validate:"min=0"` | Minimum numeric value |
| `max=N` | `validate:"max=150"` | Maximum numeric value |
| `minlen=N` | `validate:"minlen=2"` | Minimum string length |
| `maxlen=N` | `validate:"maxlen=100"` | Maximum string length |
| `pattern=REGEX` | `validate:"pattern=^[A-Z]{3}$"` | Must match the regular expression |
| `oneof=a\|b\|c` | `validate:"oneof=active\|inactive"` | Must be one of the listed values |

Combine constraints freely:

```go
type OrderInput struct {
    Status string `json:"status" validate:"required,oneof=draft|submitted|paid"`
    Code   string `json:"code"   validate:"required,pattern=^[A-Z]{3}-[0-9]{4}$"`
    Amount int    `json:"amount" validate:"required,min=1,max=10000"`
}
```

## Default Values

Use the `default` tag to provide a fallback when a field is absent or nil. The default string is automatically coerced to the field's type.

```go
type PaginationInput struct {
    Page    int  `json:"page"    default:"1"`
    PerPage int  `json:"perPage" default:"20"`
    Verbose bool `json:"verbose" default:"true"`
}

// Calling with an empty map applies defaults
result := schema.Validate(reflect.TypeOf(PaginationInput{}), map[string]any{})
// result.Data["page"] == 1
// result.Data["perPage"] == 20
// result.Data["verbose"] == true
```

Use `DefaultJSON` when a generator needs the JSON representation of a default.
String and byte-slice tags are encoded as JSON strings; other types use JSON
syntax. The function verifies decoding against the declared Go type and returns
an error that does not repeat an invalid authored value.

## Validation Options

### Type Coercion

When input comes from HTTP query strings or form data, values are often strings. Use `WithCoerce()` to automatically convert string values to the target field type.

```go
result := schema.Validate(reflect.TypeOf(CreateUserInput{}), map[string]any{
    "name":  "Alice",
    "email": "alice@example.com",
    "age":   "25", // string will be coerced to int
}, schema.WithCoerce())

// result.Data["age"] == 25 (int, not string)
```

Coercion supports: `string` to `int`, `int64`, `float64`, and `bool`.

### Field Name Prefixing

Use `WithLabel()` to prefix error field names. This is useful when validating multiple input sources (body, query, path) and you need to distinguish where an error originated.

```go
result := schema.Validate(reflect.TypeOf(CreateUserInput{}), map[string]any{},
    schema.WithLabel("body"),
)

// Errors will have fields like "body.name", "body.email"
```

Options can be combined:

```go
result := schema.Validate(reflect.TypeOf(input), values,
    schema.WithCoerce(),
    schema.WithLabel("query"),
)
```

## Error Handling

The `Result` struct contains validated data and a list of `FieldError` values. Each error identifies the field, a human-readable message, and the constraint that failed.

```go
result := schema.Validate(reflect.TypeOf(CreateUserInput{}), map[string]any{
    "name": "A",
})

for _, err := range result.Errors {
    fmt.Println(err.Field)      // "email"
    fmt.Println(err.Message)    // "is required"
    fmt.Println(err.Constraint) // "required"
    fmt.Println(err.Error())    // "email: is required"
}
```

`FieldError` implements the `error` interface, so it can be used directly as an error value.

The `Result.Data` map contains only fields that passed validation. Missing optional fields (without defaults) are omitted from `Data`.

## Struct Tag Reference

| Tag | Purpose | Example |
|---|---|---|
| `json` | Field name in the input map | `json:"user_name"` |
| `validate` | Comma-separated validation constraints | `validate:"required,email"` |
| `default` | Fallback value when field is absent | `default:"10"` |

## Best Practices

- **Combine `required` with format constraints** to enforce both presence and correctness: `validate:"required,email"`.
- **Use `WithCoerce()` for HTTP input** where all values arrive as strings (query parameters, form fields).
- **Use `WithLabel()` when validating multiple input sources** (body, query, headers) so errors clearly indicate which source failed.
- **Keep schemas as plain structs** with no methods or logic. Schemas are type descriptors, not domain objects.
- **Check `HasErrors()` before accessing `Data`**. Fields that fail validation are excluded from the `Data` map.
- **Use `oneof` for enums** rather than validating in application code.
- **Use `pattern` sparingly**. Prefer built-in constraints (`email`, `uuid`, `url`) over equivalent regex patterns.

## Contract and compatibility

See the [struct validation specification](../specs/struct-validation.json), the
[declaration-faults ADR](adr/0001-declaration-faults-are-not-validation-results.md),
and [support evidence](../README.md#support-and-contract). The package is stable
and maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
