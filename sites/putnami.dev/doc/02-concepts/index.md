# Concepts

Putnami is a workspace framework for building full-stack applications.
One CLI orchestrates TypeScript and Go projects, plus explicitly opted-in
experimental Python projects. Python does not carry a production, default, or
parity promise.

This page is the mental map. Not the command surface. Just the layers that make the system legible.

```text
     ┌──────────────────────────────────────────────────────────┐
     │                       Platform                           │
     │    (Environments, delivery, networking, observability)   │
     └──────────────────────────────────────────────────────────┘
                             ▲
                             │ makes real
                             │
     ┌──────────────────────────────────────────────────────────┐
     │                      Framework                           │
     │       (Web, API, auth, data — app patterns)              │
     └──────────────────────────────────────────────────────────┘
                             ▲
                             │ uses
                             │
     ┌──────────────────────────────────────────────────────────┐
     │                       Runtime                            │
     │       (DI, config, lifecycle — the execution layer)      │
     └──────────────────────────────────────────────────────────┘
                             ▲
                             │ operates on
                             │
     ┌──────────────────────────────────────────────────────────┐
     │                        Workspace                         │
     │                                                          │
     │    ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
     │    │   Project   │  │   Project   │  │   Project   │     │
     │    │  (service)  │  │  (library)  │  │   (site)    │     │
     │    └─────────────┘  └─────────────┘  └─────────────┘     │
     │                                                          │
     │    ┌─────────────┐  ┌─────────────┐                      │
     │    │ Extension   │  │ Extension   │  ...                 │
     │    └─────────────┘  └─────────────┘                      │
     └──────────────────────────────────────────────────────────┘
```

---

## Workspace

The root of everything.

A workspace is a repository root identified by a `putnami.workspace.json` file.
It defines discovery boundaries, shared defaults, active extensions, and orchestration.

A workspace:

- groups multiple projects
- declares which extensions are active
- defines scopes, aliases, and shared options
- is the unit Putnami operates on locally and in automation

The workspace tracks the dependency graph between projects.
When you change a library, `putnami build --impacted` rebuilds everything that depends on it — and nothing else.

## Project

A project is a node in the workspace graph.
It is something Putnami can identify, understand, and run jobs against.

Common project shapes include:

- **App / service** — Deployable. Has an entry point. Becomes a running process.
- **Library** — Importable. Shared code used by apps or other libraries.
- **Extension / template / support package** — Extends how the workspace works.

Projects can be written in different languages within the same workspace. A TypeScript web app and a Go API service live side by side, built and tested with the same CLI.

## Extension

The extension mechanism of Putnami.

Extensions extend Putnami itself.

They add capabilities such as:

- tooling steps (build, test, lint, serve, publish)
- language support
- frameworks
- platform integrations

Extensions are discovered from workspace projects, installed dependencies, or explicit workspace references.
Putnami reads their manifests, understands what commands they provide, and runs their jobs as subprocesses.

Extensions define what the CLI can do.
Frameworks and project code define what applications look like.

## Runtime

The execution layer between Putnami and your application code.

The runtime manages:

- **Dependency injection** — a hierarchical container that wires services together
- **Configuration** — type-safe config from YAML files and environment variables
- **Lifecycle** — startup, shutdown, health checks
- **Context** — request-scoped state, structured logging, telemetry

You rarely interact with the runtime directly. When you declare a service, configure a database connection, or add middleware — the runtime handles the wiring.

Local development already lives here.
The platform carries this same runtime contract into preview and production.

## Framework

Reusable patterns for building applications.

Frameworks define how apps are written — same architectural principles, language-native implementations.

| Concern | TypeScript | Go |
|---------|-----------|-----|
| HTTP server & routing | `@putnami/application` | `go.putnami.dev/http` |
| React SSR | `@putnami/web` | — |
| Database (PostgreSQL) | `@putnami/database` | `go.putnami.dev/database` |
| Document collections (NoSQL) | `@putnami/document` | — |
| Auth & authorization | `@putnami/application` | `go.putnami.dev/security` |
| Validation | `@putnami/runtime` | `go.putnami.dev/schema` |
| Events | `@putnami/events` | `go.putnami.dev/events` |
| HTTP client | `@putnami/client` | `go.putnami.dev/client` |
| Object storage | `@putnami/storage` | `go.putnami.dev/storage` |

You compose only the frameworks your app needs.

Frameworks may have their own internal extension systems.
Those are framework concerns, not Putnami concepts.

## Environment

An isolated instance of a system at a particular revision and configuration.

Typical environments include:

- local development
- ephemeral preview environments (one per branch)
- shared staging
- production

Each environment has its own deployment, configuration, data boundary, and observable state.

The important point is not the label.
It is that an environment is derivable from code instead of manually curated.

## Platform

The operational layer that makes the environment model real.

The platform:

- manages environments
- handles delivery and deployment
- enforces configuration, networking, and observability
- converges local, preview, and production around the same system contract

Some platform-adjacent pieces already exist today in CI, publishing, and release automation.
The managed environment layer is the next part Putnami is building.

## Protocols

The contracts that keep every layer honest.

Each boundary in the system — workspace configuration, extension manifests, job events, templates, config schemas, infra requirements, migrations, operational endpoints — is a small, versioned protocol with a JSON schema, a fixture corpus, and conformance tests.

Protocols are why the system stays polyglot without drifting:

- the CLI, the Go framework, and the TypeScript framework implement the **same contract**, verified against **shared fixtures**
- automation consumes structured output (`putnami … --output=jsonl`) instead of scraping text
- configuration stays reviewable JSON instead of imperative code

The protocol packages live in [`protocols`](https://github.com/putnami/putnami/tree/main/protocols), each with its own README, schemas, and fixtures.

## Summary

| Concept | Role |
|---------|------|
| **Workspace** | The system boundary |
| **Project** | A unit in the graph |
| **Extension** | What extends Putnami |
| **Runtime** | The execution layer (DI, config, lifecycle) |
| **Framework** | How apps are structured (web, API, data, auth) |
| **Environment** | An isolated system instance |
| **Platform** | The operational layer |
| **Protocols** | The contracts between every layer |

---

Next:

- [Support status](/docs/support) — what each package and protocol publicly commits to
- [CLI telemetry and your rights](/docs/concepts/cli-telemetry)
- [Remote cache trust](/docs/concepts/remote-cache-trust)
- [Getting Started](/docs/getting-started)
- [How-To](/docs/how-to)
- [Frameworks](/docs/frameworks)
- [Principles](/docs/principles)
