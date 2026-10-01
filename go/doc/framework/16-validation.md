# Validation

`go.putnami.dev/schema` provides struct-tag-based validation with type coercion and default values.

## Validating data

Validate a `map[string]any` against a Go struct type:

```go
import (
    "reflect"
    "go.putnami.dev/schema"
)

type CreateUserInput struct {
    Name  string `json:"name" validate:"required,minlen=2,maxlen=100"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age" validate:"min=0,max=150"`
}

result := schema.Validate(
    reflect.TypeOf(CreateUserInput{}),
    map[string]any{
        "name":  "Jane",
        "email": "not-an-email",
        "age":   -5,
    },
)

if result.HasErrors() {
    for _, err := range result.Errors {
        fmt.Printf("%s: %s\n", err.Field, err.Message)
        // email: must be a valid email address
        // age: must be at least 0
    }
}
```

## Validation tags

Add validation rules to struct fields using the `validate` tag:

```go
type Product struct {
    ID          string  `json:"id" validate:"required,uuid"`
    Name        string  `json:"name" validate:"required,minlen=1,maxlen=200"`
    Description string  `json:"description" validate:"maxlen=2000"`
    Price       float64 `json:"price" validate:"required,min=0"`
    Category    string  `json:"category" validate:"required,oneof=electronics|clothing|food"`
    URL         string  `json:"url" validate:"url"`
}
```

### Available validators

| Tag | Description | Example |
|-----|-------------|---------|
| `required` | Must be present and non-zero | `validate:"required"` |
| `uuid` | UUID v4 format | `validate:"uuid"` |
| `email` | Valid email address | `validate:"email"` |
| `url` | Valid URL | `validate:"url"` |
| `min=N` | Minimum numeric value | `validate:"min=0"` |
| `max=N` | Maximum numeric value | `validate:"max=100"` |
| `minlen=N` | Minimum string length | `validate:"minlen=2"` |
| `maxlen=N` | Maximum string length | `validate:"maxlen=255"` |
| `pattern=REGEX` | Must match regular expression | `validate:"pattern=^[a-z]+$"` |
| `oneof=a\|b\|c` | Must be one of the listed values | `validate:"oneof=active\|inactive"` |

Combine multiple validators with commas:

```go
validate:"required,minlen=2,maxlen=100"
```

## Default values

Set default values for optional fields:

```go
type SearchParams struct {
    Query    string `json:"query" validate:"required"`
    Page     int    `json:"page" default:"1" validate:"min=1"`
    PageSize int    `json:"pageSize" default:"20" validate:"min=1,max=100"`
    Sort     string `json:"sort" default:"relevance" validate:"oneof=relevance|date|price"`
}
```

Defaults are applied before validation, so fields with defaults are always populated.

## Type coercion

Enable automatic type coercion to convert string values to their target types:

```go
result := schema.Validate(
    reflect.TypeOf(SearchParams{}),
    map[string]any{
        "query":    "shoes",
        "page":     "2",      // string → int
        "pageSize": "50",     // string → int
    },
    schema.WithCoerce(),
)

// result.Data["page"] == 2 (int, not "2")
```

## Validation result

```go
type Result struct {
    Data   map[string]any   // Validated and coerced values
    Errors []FieldError     // Validation errors
}

type FieldError struct {
    Field   string // Field name
    Message string // Error description
}

result.HasErrors() // true if any validation errors
```

## Validation options

| Option | Description |
|--------|-------------|
| `WithCoerce()` | Enable automatic type coercion (string → int, etc.) |
| `WithLabel(prefix)` | Prefix error field names (e.g., `"body."` → `"body.name"`) |

## Using with HTTP endpoints

### Manual validation

```go
func createUser(ctx *fhttp.Context) *fhttp.Response {
    var input map[string]any
    if err := ctx.Body(&input); err != nil {
        return fhttp.JSONStatus(400, map[string]string{"error": "invalid JSON"})
    }

    result := schema.Validate(
        reflect.TypeOf(CreateUserInput{}),
        input,
        schema.WithCoerce(),
    )
    if result.HasErrors() {
        return fhttp.JSONStatus(422, map[string]any{"errors": result.Errors})
    }

    // Use result.Data for validated values
    name := result.Data["name"].(string)
    email := result.Data["email"].(string)
    // ...
}
```

### With the endpoint builder

The endpoint builder integrates validation automatically:

```go
endpoint := fhttp.Endpoint("POST", "/users").
    Body(reflect.TypeOf(CreateUserInput{})).
    Handle(func(ctx *fhttp.EndpointContext) *fhttp.Response {
        // ctx.ValidatedBody contains validated and coerced data
        name := ctx.ValidatedBody["name"].(string)
        return fhttp.JSONStatus(201, map[string]string{"name": name})
    })
```

See [HTTP & Middleware](/docs/frameworks/go/http#endpoint-builder) for details.

## Nested struct validation

Nested structs are validated recursively:

```go
type Address struct {
    Street string `json:"street" validate:"required"`
    City   string `json:"city" validate:"required"`
    Zip    string `json:"zip" validate:"required,pattern=^[0-9]{5}$"`
}

type CreateUserInput struct {
    Name    string  `json:"name" validate:"required"`
    Email   string  `json:"email" validate:"required,email"`
    Address Address `json:"address" validate:"required"`
}
```

## Declaration faults

A struct tag is not checked by the compiler, so a typo silently changes what is
enforced. It does not here: an unknown constraint name or an uncompilable pattern
is reported as a field error in its own right, before any value is looked at and
whether or not a value was supplied.

```go
type Input struct {
    Name string `json:"name" validate:"requred"` // typo
}
// → FieldError{Field: "name", Constraint: "requred", Message: "unknown constraint"}
// The field does NOT silently become optional.
```

`Validate` never panics on a caller-supplied type: a nil or non-struct type
returns a structured schema error, and `*T` validates exactly like `T`.

## Absent fields and partial updates

A field that is absent or nil is skipped after the required check, so a partial
update validates only the fields it supplies:

```go
type UpdateTaskInput struct {
    Title  string `json:"title,omitempty"  validate:"maxlen=200"`
    Status string `json:"status,omitempty" validate:"oneof=pending|in_progress|completed"`
}
// A body carrying only {"status":"completed"} does not fail on Title.
```

A `default` tag fills an absent field before the required check runs, so a
defaulted field satisfies `required`.

Only fields that passed every constraint appear in `Result.Data`, so a caller
that reads the data map cannot act on a rejected value.

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — endpoint validation
- [OpenAPI](/docs/frameworks/go/openapi) — validation tags in API specs
- [Errors](/docs/frameworks/go/errors) — validation error responses
## Support and contract

`go.putnami.dev/schema` is `stable` in the workspace support catalog. Its
behavior is defined by the struct validation specification and its accepted
decision record next to the package source (`go/framework/schema/specs/` and
`go/framework/schema/doc/adr/`). Before v1.0.0 a minor `0.x` release may still
contain a documented breaking change; strict compatibility between every pre-1.0
minor is not promised.
