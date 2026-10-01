# Go Getting Started

Use Go when the first project is a backend service, worker, platform-adjacent component, or library where explicit wiring and predictable runtime behavior matter.

This path starts with the workspace template, then moves into the Go framework primitives.

## Pick the project shape

| You need... | Template | What you get |
|-------------|----------|--------------|
| An HTTP service | `go-server` | Service entry point, operational endpoints (`/readyz`, `/version`, `/_/health`), tests, Dockerfile |
| A shared Go package | `go-library` | Go module, exported function, tests, workspace wiring |

If you are starting from an empty directory:

```bash
putnami init
```

## Create the project

For a service:

```bash
putnami projects create api --template go-server
putnami test api
putnami serve api
```

For a library:

```bash
putnami projects create domain --template go-library
putnami test domain
```

## What to look at first

Start with the service skeleton, then add capabilities in this order:

1. [Overview](/docs/frameworks/go/overview) for the module map and framework shape.
2. [HTTP & Middleware](/docs/frameworks/go/http) for routing, middleware, handlers, and responses.
3. [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) for startup, shutdown, and capability ownership.
4. [Configuration](/docs/frameworks/go/configuration) and [Dependency Injection](/docs/frameworks/go/dependency-injection) before wiring spreads across package globals.
5. [Platform endpoints](/docs/frameworks/go/platform-endpoints), [Logging](/docs/frameworks/go/logging), and [Telemetry](/docs/frameworks/go/telemetry) before production.

## Daily loop

```bash
putnami test api
putnami serve api
putnami lint,test,build --impacted
```

`putnami serve` keeps the Go project inside the same workspace workflow as TypeScript, Python, and tooling projects.

## Read next

- [Go overview](/docs/frameworks/go/overview) for the package map.
- [Extension](/docs/frameworks/go/extension) for Go-specific build, test, serve, and packaging behavior.
- [Tooling & Workspace](/docs/tooling-&-workspace) for project selection, impacted work, and caching.
