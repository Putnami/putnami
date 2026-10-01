# go.putnami.dev/config

Typed, multi-source configuration loading with YAML auto-discovery, env vars, and DI integration.

## Quick Start

```go
import "go.putnami.dev/config"

type ServerOptions struct {
    Host string `json:"host" default:"localhost"`
    Port int    `json:"port" default:"8080" env:"PORT"`
}

var ServerConfig = config.Config[ServerOptions]("server")

cfg, err := config.Load(ServerConfig)
// Auto-discovers conf/.env.yaml, conf/.env.{APP_ENV}.yaml, CONFIG_DATA env var
// cfg.Host == "localhost", cfg.Port == 8080 (or PORT env var)
```

## Struct Tags

| Tag | Purpose | Example |
|-----|---------|---------|
| `json` | Key in config tree | `json:"host"` |
| `default` | Fallback value | `default:"8080"` |
| `env` | Env var (highest priority) | `env:"PORT"` |
| `sensitive` | Mark field as secret (storage routing + log redaction) | `sensitive:"true"` |

Supported scalar types: `string`, `int`, `int64`, `float64`, `bool`,
`time.Duration`, and pointers to those scalar types for optional values. Map and
YAML sources also recursively populate nested structs, slices, and typed maps
when their keys and values can be mapped to the target types.

Fields tagged `sensitive:"true"` are routed to the secrets store at publish time and surfaced via `config.SensitiveFields(def)` / `config.IsSensitiveField(def, name)` at runtime so loggers and serializers can redact them. See [ADR 0001: `sensitive` decides the store](../../../protocols/config/doc/adr/0001-sensitive-decides-the-store.md).

## Auto-Discovery

When `Load()` is called with no explicit sources, it automatically discovers:

| Source | Priority | Description |
|--------|----------|-------------|
| `conf/.env.yaml` | 10 | Base config file |
| `conf/.env.{APP_ENV}.yaml` | 20 | Environment-specific overrides |
| `.gen/conf/.env.{APP_ENV}.yaml` | 30 | Build-merged config from workspace dependencies |
| `conf/.secrets.{APP_ENV}.yaml` | 35 | Local secrets (gitignored by template default) |
| Registered source discoverers | source-defined | Optional compile-time integrations such as a remote config client |
| `CONFIG_DATA` env var | 60 | Inline YAML for containers |
| `env` struct tags | 80 | Environment variable overrides (always active) |

Set `APP_ENV` to control which environment file is loaded (defaults to `"local"`).

Remote config and secrets are not bundled in core. A Go workload opts in by
registering a source discoverer at compile time. The discoverer returns `nil`
when its source is not enabled for the process:

```go
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

Without that registration, core discovery is limited to local files,
`CONFIG_DATA`, and env-tag overrides.

## Config Sources

```go
// Explicit sources (auto-discovery is skipped)
cfg, _ := config.Load(ServerConfig,
    config.NewYAMLFileSource("custom.yaml", 50),
    config.NewMapSource("defaults", 10, map[string]any{"server": map[string]any{"host": "0.0.0.0"}}),
)

// CONFIG_DATA for containerized deployments
// CONFIG_DATA='server:\n  port: 9090' ./myapp
```

## DI Integration

```go
// Register config as a DI provider
a := app.New("my-service")
a.Provide(config.Provide(ServerConfig))

// Resolve in invoker
a.InvokeFunc(func(cfg *ServerOptions) {
    fmt.Println(cfg.Port)
})
```

## Contributing config from a library

A library that owns a config block publishes it through any workload that
composes it by implementing `app.ConfigContributor` on its plugin and returning
the type-erased `Descriptor` of each definition it owns:

```go
var CoreConfig = config.Config[CoreOptions]("core")

func (p *Plugin) ConfigDefinitions() []config.Descriptor {
    return []config.Descriptor{CoreConfig.Descriptor()}
}
```

At build time the describe phase walks the module tree, reflects each contributed
schema, and merges the blocks — and the secrets their `sensitive` fields declare
— into the workload's published `schema/config.json`, the same way migration
sources aggregate from deps. The block's path must be one the workload does not
already define (a collision fails the build), and the library still loads its
values at runtime the usual way (`config.Load(CoreConfig)` /
`config.Provide(CoreConfig)`).

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [typed
configuration specification](specs/typed-configuration.json) and [precedence
ADR](doc/adr/0001-precedence-and-fail-loud-mapping.md). Before v1.0, follow the
workspace [migration-based compatibility policy](../../../RELEASE.md); do not
infer strict compatibility between every `0.x` minor.
