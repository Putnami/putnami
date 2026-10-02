# Configuration

`go.putnami.dev/config` provides typed, multi-source configuration loading for the Putnami Go framework. It merges values from environment variables, static maps, and custom sources with priority-based resolution, and integrates with the DI container.

## Defining a Configuration

Define a configuration block by creating a Go struct and binding it to a path in the config tree using `config.Config`:

```go
package myapp

import "go.putnami.dev/config"

type ServerOptions struct {
    Host string `json:"host" default:"localhost"`
    Port int    `json:"port" default:"8080" env:"PORT"`
}

var ServerConfig = config.Config[ServerOptions]("server")
```

The `Definition` returned by `config.Config` captures the dot-notation path and the struct type. It is used with `Load`, `Token`, and `Provide`.

## Struct Tags

Fields support four struct tags that control how values are resolved:

| Tag | Purpose | Example |
|-----|---------|---------|
| `json` | Maps the field to a key in the config tree | `json:"host"` |
| `default` | Fallback value when no source provides one | `default:"8080"` |
| `env` | Environment variable name (highest precedence) | `env:"PORT"` |
| `sensitive` | Mark field as a secret | `sensitive:"true"` |

Supported scalar types: `string`, `int`, `int64`, `float64`, `bool`.

### Structured Values

Map and YAML sources can populate nested structs, slices, and typed maps. Map
keys and values use the same conversion rules as other config fields, so a
decoded `map[string]any` can populate maps such as `map[string]string`,
`map[string]Backend`, or `map[int]string`.

```go
type Backend struct {
    Host string `json:"host"`
    Port int    `json:"port"`
}

type ServiceConfig struct {
    Labels   map[string]string  `json:"labels"`
    Backends map[string]Backend `json:"backends"`
}
```

For `bool` fields, the values `"true"`, `"1"`, and `"yes"` are treated as true.

The `sensitive` tag has two effects: at publish time it routes the value to the secrets store instead of the plaintext config store, and at runtime it is surfaced via `SensitiveFields` / `IsSensitiveField` so loggers can redact the value. See [Sensitive fields](#sensitive-fields) below.

## Loading Configuration

Call `config.Load` with a definition and zero or more sources:

```go
cfg, err := config.Load(ServerConfig)
if err != nil {
    log.Fatal(err)
}
fmt.Println(cfg.Host) // "localhost" (from default tag)
fmt.Println(cfg.Port) // 8080 (from default tag)
```

When no sources are provided, fields fall back to their `default` tag values. Fields with an `env` tag read from the corresponding environment variable regardless of sources.

## Sources

A source is any value implementing the `Source` interface:

```go
type Source interface {
    Name() string
    Priority() int
    Load() (map[string]any, error)
}
```

Sources return a nested `map[string]any` tree. When multiple sources are provided, they are merged by priority (higher priority wins). Nested maps are deep-merged so that partial overrides work correctly.

### MapSource

`MapSource` provides configuration from an in-memory map. Useful for tests and programmatic defaults.

```go
source := config.NewMapSource("defaults", 50, map[string]any{
    "server": map[string]any{
        "host": "0.0.0.0",
        "port": 3000,
    },
})

cfg, err := config.Load(ServerConfig, source)
// cfg.Host == "0.0.0.0", cfg.Port == 3000
```

### EnvSource

`EnvSource` is a built-in source for environment variables. Individual fields opt in to environment variable resolution via the `env` struct tag.

```go
source := config.NewEnvSource("APP")
```

Environment variables set via `env` tags always take precedence over map-based sources, regardless of source priority.

### Custom Sources

Implement the `Source` interface to load from files, remote stores, or any other origin:

```go
type YAMLSource struct {
    path string
}

func (s *YAMLSource) Name() string              { return "yaml:" + s.path }
func (s *YAMLSource) Priority() int              { return 50 }
func (s *YAMLSource) Load() (map[string]any, error) {
    // Parse YAML file and return nested map
}
```

## Priority and Merge Order

Sources are sorted by priority (higher wins). When two sources provide the same key, the higher-priority value is used. Nested maps are deep-merged so keys from lower-priority sources are preserved when absent from higher-priority ones.

Resolution order for a given field:

1. `env` tag (environment variable) -- always wins if set
2. Highest-priority source value
3. Lower-priority source values (deep-merged)
4. `default` tag -- used only when no source provides the key

```go
low := config.NewMapSource("low", 10, map[string]any{
    "server": map[string]any{"host": "low-priority"},
})
high := config.NewMapSource("high", 90, map[string]any{
    "server": map[string]any{"host": "high-priority"},
})

cfg, err := config.Load(ServerConfig, low, high)
if err != nil {
    log.Fatal(err)
}
// cfg.Host == "high-priority"
```

## Nested Paths

The definition path supports dot notation to reach deeply nested sections of the config tree:

```go
var APIServer = config.Config[ServerOptions]("services.api.server")

source := config.NewMapSource("app", 50, map[string]any{
    "services": map[string]any{
        "api": map[string]any{
            "server": map[string]any{
                "host": "api.example.com",
                "port": 443,
            },
        },
    },
})

cfg, err := config.Load(APIServer, source)
if err != nil {
    log.Fatal(err)
}
// cfg.Host == "api.example.com"
```

## Dependency Injection Integration

The config package integrates with `go.putnami.dev/inject`. Use `config.Token` to get a DI token and `config.Provide` to register a config loader as a DI provider.

```go
// Create a DI token for the config definition
token := config.Token(ServerConfig)

// Register a provider that loads config when resolved
reg := config.Provide(ServerConfig, fileSource, envSource)

// Use with an inject.Container
container := inject.New(reg)
```

`config.Provide` returns a non-lazy singleton `inject.Registration`. A normal
container start resolves it eagerly, so source or mapping failures stop startup
instead of surfacing on the first later lookup.

## Sensitive fields

Mark fields whose values are secrets with `sensitive:"true"`:

```go
type DatabaseOptions struct {
    Host     string `json:"host" default:"localhost"`
    Port     int    `json:"port" default:"5432"`
    Password string `json:"password" env:"DB_PASSWORD" sensitive:"true"`
}
```

At publish time the tag routes the value to the secrets store
(`PUT /api/secrets`) instead of the plaintext config store. At runtime,
the framework exposes which fields are sensitive via:

```go
config.SensitiveFields(DatabaseConfig)        // -> []string{"password"}
config.IsSensitiveField(DatabaseConfig, "password") // -> true
```

Use these in custom loggers / serializers to redact secret values from
output. The protocol's [ADR 0001: `sensitive` decides the
store](../../../../protocols/config/doc/adr/0001-sensitive-decides-the-store.md)
states the routing and write-validation rules.

### Secrets in local development

A developer running locally with no config-server uses a separate file:

| File | Committed? | Holds |
|------|------------|-------|
| `conf/.env.{APP_ENV}.yaml` | yes | plaintext config |
| `conf/.secrets.{APP_ENV}.yaml` | no (gitignored) | local secrets |

Both files are auto-discovered. The secrets file sits at priority 35,
just above the build-merged config file (30), so secret values override
non-sensitive defaults but operational env-var injection still wins.

### Remote config and secrets

Remote config and secrets are not part of the core `go.putnami.dev/config`
module. Core exposes a compile-time registry for optional source discoverers.
A discoverer returns the `Source` to add, or `nil` when that source is not
enabled for the process:

```go
package main

import (
    "os"

    "go.putnami.dev/config"
)

func init() {
    config.RegisterSourceDiscoverer(func() config.Source {
        path := os.Getenv("REMOTE_CONFIG_FILE")
        if path == "" {
            return nil // not enabled for this process
        }
        return config.NewYAMLFileSource(path, 50)
    })
}
```

Registered discoverers run after the local files and before `CONFIG_DATA`;
each source's `Priority()` places it in the merge. Without a registration,
`config.Load` boots from local YAML, local secrets, `CONFIG_DATA`, and env-tag
overrides only.

## Error Handling

`config.Load` returns typed errors with the following codes:

| Code | Meaning |
|------|---------|
| `config.source` | A source failed to load |
| `config.path` | The config path resolved to a non-object value |
| `config.mapping` | Struct mapping failed (e.g., target is not a pointer to struct) |

## Best Practices

- Define config variables at package level so they can be referenced by `Token` and `Provide`.
- Always set `default` tags for fields that have reasonable defaults. This makes zero-source loading safe.
- Use `env` tags for secrets and deployment-specific values (ports, URLs, credentials) so they can be overridden without changing config files.
- Keep config paths short and organized by domain: `"server"`, `"database"`, `"auth.oauth"`.
- In tests, use `NewMapSource` to provide deterministic config without touching the environment.

## Contract and compatibility

See the [typed configuration specification](../specs/typed-configuration.json),
[precedence ADR](adr/0001-precedence-and-fail-loud-mapping.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
