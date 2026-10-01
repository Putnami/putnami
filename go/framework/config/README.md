# Configuration

The `config` package provides multi-source configuration loading with support for YAML files, environment variables, programmatic defaults, source priority ordering, and DI integration.

## Defining Configs

Define typed configuration blocks with a path into the config tree:

```go
import "go.putnami.dev/config"

type ServerConfig struct {
    Host string `json:"host" default:"localhost"`
    Port int    `json:"port" default:"8080" env:"PORT"`
}

var serverDef = config.Config[ServerConfig]("server")
```

### Struct Tags

| Tag | Description |
|-----|-------------|
| `json:"name"` | Field name in the config tree |
| `default:"value"` | Default value when no source provides one |
| `env:"VAR"` | Environment variable override (highest priority) |

### Structured Values

Map and YAML sources can populate nested structs, slices, and typed maps. Map
keys and values are converted using the same rules as other config fields, so
a decoded `map[string]any` can populate maps such as `map[string]string`,
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

## Loading Config

When called with no explicit sources, `Load` automatically discovers configuration:

```go
cfg, err := config.Load(serverDef)
// Discovers: conf/.env.yaml, conf/.env.{APP_ENV}.yaml, local secrets,
// registered optional sources, CONFIG_DATA env var
// Plus env struct tags always apply
```

Pass explicit sources to override auto-discovery:

```go
cfg, err := config.Load(serverDef,
    config.NewYAMLFileSource("custom.yaml", 50),
)
```

## Auto-Discovery

When `Load()` is called with no sources, it scans for configuration automatically:

| Source | Priority | Description |
|--------|----------|-------------|
| `conf/.env.yaml` | 10 | Base config file |
| `conf/.env.{APP_ENV}.yaml` | 20 | Environment-specific overrides |
| `.gen/conf/.env.{APP_ENV}.yaml` | 30 | Build-merged config from workspace dependencies |
| `conf/.secrets.{APP_ENV}.yaml` | 35 | Local secrets (gitignored by template default) |
| Registered source discoverers | source-defined | Optional compile-time integrations such as a remote config client |
| `CONFIG_DATA` env var | 60 | Inline YAML for containers |
| `env` struct tags | 80 | Environment variable overrides (always active) |

Remote config and secrets are not bundled in `go.putnami.dev/config`. A
workload opts in at compile time by registering a source discoverer. The
discoverer runs after the local files and before `CONFIG_DATA`, and returns
`nil` when its source is not enabled for the process:

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

To make a slow startup interruptible — e.g. a `SIGTERM` while a required config
source is still loading — load with a context: `config.LoadContext(ctx, def,
sources...)` (or `config.ProvideContext(ctx, def, sources...)` for DI).
`config.Load`/`config.Provide` use a background context and keep their existing,
non-cancellable behavior.

### Environment Detection

Set `APP_ENV` to control which config file is loaded. Defaults to `"local"`.

```bash
APP_ENV=production ./myapp   # loads conf/.env.production.yaml
APP_ENV=test ./myapp         # loads conf/.env.test.yaml
```

Use `config.Environment()` to read the current environment in code.

### CONFIG_DATA

For containerized deployments (Cloud Run, K8s), pass inline YAML via the `CONFIG_DATA` environment variable:

```bash
CONFIG_DATA='server:
  host: 0.0.0.0
  port: 9090' ./myapp
```

## Sources

### YAMLFileSource

Loads configuration from a YAML file:

```go
source := config.NewYAMLFileSource("config.yaml", 50)
// Returns nil (no error) if file does not exist
```

### YAMLDataSource

Loads configuration from raw YAML bytes:

```go
source := config.NewYAMLDataSource("inline", 60, []byte(yamlContent))
```

### MapSource

Provides configuration from a static map:

```go
source := config.NewMapSource("defaults", 50, map[string]any{
    "server": map[string]any{
        "host": "0.0.0.0",
        "port": 3000,
    },
})
```

### EnvSource

Reads from environment variables. Individual fields opt-in via the `env` struct tag:

```go
source := config.NewEnvSource("APP") // optional prefix
```

### Custom Source

Implement the `Source` interface:

```go
type Source interface {
    Name() string
    Priority() int
    Load() (map[string]any, error)
}
```

## Nested Paths

Use dot-notation to navigate deep config trees:

```go
var apiConfig = config.Config[ServerConfig]("services.api.server")

source := config.NewMapSource("file", 50, map[string]any{
    "services": map[string]any{
        "api": map[string]any{
            "server": map[string]any{
                "host": "api.example.com",
                "port": 443,
            },
        },
    },
})
```

## DI Integration

Register config in the DI container:

```go
// Create a DI token for the config
token := config.Token(serverDef)

// Create a DI registration that loads config at container start
app.Provide(config.Provide(serverDef, sources...))
```

The provider is a non-lazy singleton: application startup resolves it and fails
immediately if a required source or typed mapping is invalid.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/config` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [typed configuration specification](specs/typed-configuration.json) and
[precedence ADR](doc/adr/0001-precedence-and-fail-loud-mapping.md) define the
contract. Regression evidence covers [source precedence and typed
mapping](config_test.go), [strict conversions](config_extra_test.go), [source
discovery](discover_test.go), [sensitive values](sensitive_test.go), and [YAML
sources](yaml_test.go).
