# Configuration

`go.putnami.dev/config` provides multi-source configuration loading with struct-tag-driven mapping, environment variable overrides, and DI integration.

## Defining configuration

Define a typed configuration at a path in the config tree:

```go
import "go.putnami.dev/config"

type ServerConfig struct {
    Host    string `json:"host" default:"localhost"`
    Port    int    `json:"port" default:"8080" env:"PORT"`
    Debug   bool   `json:"debug" default:"false" env:"DEBUG"`
    Timeout int64  `json:"timeout" default:"30" env:"TIMEOUT"`
}

var serverDef = config.Config[ServerConfig]("server")
```

The path uses dot-notation for nested structures:

```go
// Navigates to root["services"]["api"]["server"]
var apiServerDef = config.Config[ServerConfig]("services.api.server")
```

## Struct tags

| Tag | Description | Example |
|-----|-------------|---------|
| `json:"name"` | Field name in the config tree | `json:"port"` |
| `default:"value"` | Default value when no source provides one | `default:"8080"` |
| `env:"VAR"` | Environment variable override (highest priority) | `env:"PORT"` |

Priority order (highest to lowest):
1. Environment variables (via `env` tag)
2. Explicit source data
3. Default values (via `default` tag)

## Loading configuration

```go
cfg, err := config.Load(serverDef,
    config.NewMapSource("file", 50, map[string]any{
        "server": map[string]any{
            "host": "0.0.0.0",
            "port": 3000,
        },
    }),
)
// cfg.Host == "0.0.0.0"    (from source)
// cfg.Port == 3000          (from source, unless PORT env var is set)
// cfg.Debug == false        (from default tag)
```

Multiple sources are merged in priority order (higher priority wins):

```go
cfg, err := config.Load(serverDef,
    config.NewMapSource("defaults", 10, defaultData),
    config.NewMapSource("file", 50, fileData),
    config.NewMapSource("overrides", 90, overrideData),
)
```

## Sources

### MapSource

Provides configuration from a static map:

```go
source := config.NewMapSource("file", 50, map[string]any{
    "server": map[string]any{
        "host": "0.0.0.0",
        "port": 3000,
    },
    "database": map[string]any{
        "dsn": "postgres://localhost/mydb",
    },
})
```

### EnvSource

Reads from environment variables. Individual fields opt in via the `env` struct tag:

```go
source := config.NewEnvSource("APP") // prefix: APP_
// "server.port" with env:"PORT" → checks APP_PORT

source := config.NewEnvSource("") // no prefix
// "server.port" with env:"PORT" → checks PORT
```

The `EnvSource` has priority `80` by default, so environment variables take precedence over most file-based sources.

### Custom sources

Implement the `Source` interface:

```go
type Source interface {
    Name() string
    Priority() int
    Load() (map[string]any, error)
}
```

## Type conversion

Fields are automatically converted from source values:

| Target type | Conversion |
|-------------|-----------|
| `string` | Direct assignment or `fmt.Sprint` |
| `int`, `int64` | Parsed from string or numeric value |
| `float64` | Parsed from string or numeric value |
| `bool` | `"true"`, `"1"`, `"yes"` → `true`; others → `false` |

## DI integration

### Config tokens

Create a DI token from a config definition:

```go
token := config.Token(serverDef)
// token = inject.Named[ServerConfig]("config:server")
```

### Config provider

Register a config definition as a DI provider:

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/config"
)

a := app.New("my-service")
a.Module.Provide(config.Provide(serverDef,
    config.NewMapSource("file", 50, yamlData),
))
```

The config is loaded when the DI container starts. Other providers can depend on the config type:

```go
// Resolved automatically via DI
func NewHTTPServer(cfg ServerConfig) *fhttp.ServerPlugin {
    return fhttp.NewServerPlugin(fhttp.ServerConfig{Port: cfg.Port})
}

a.ProvideFunc(NewHTTPServer)
```

## Nested configuration

```go
type AppConfig struct {
    Server   ServerConfig   `json:"server"`
    Database DatabaseConfig `json:"database"`
    Features FeatureFlags   `json:"features"`
}

type DatabaseConfig struct {
    DSN         string `json:"dsn" env:"DATABASE_URL"`
    MaxConns    int    `json:"maxConns" default:"10"`
    MaxIdleTime string `json:"maxIdleTime" default:"30m"`
}

type FeatureFlags struct {
    NewUI      bool `json:"newUI" default:"false" env:"FEATURE_NEW_UI"`
    BetaAccess bool `json:"betaAccess" default:"false"`
}

var appDef = config.Config[AppConfig]("app")
```

## Error codes

| Code | Description |
|------|-------------|
| `config.source` | Source `Load()` failed |
| `config.path` | Path does not resolve to an object |
| `config.mapping` | Struct mapping failed (type conversion error) |

## Related guides

- [Dependency Injection](/docs/frameworks/go/dependency-injection) — using config with DI
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — application setup

## Support and compatibility

`go.putnami.dev/config` is a public, documented, maintained package classified
`stable`. Its typed-configuration specification and accepted precedence ADR
live next to the package source. Before v1.0.0, minor `0.x` releases may still
contain documented breaking changes; strict compatibility across every pre-1.0
minor is not promised.
