# Templates

Templates are project scaffolds that let you create fully configured projects in one command. They are decoupled from extensions — each template is defined by a `putnami.template.json` manifest and can be installed, versioned, and published independently.

## Creating a project from a template

```bash
putnami projects create my-app --template typescript-web
```

The `--template` flag is required. Options:

- `--template <name>` — Template to scaffold from (required)
- `--path <path>` — Custom project path (default: determined by workspace scopes)

## Official templates

Each language extension ships its own templates:

| Template | Extension | Description |
|----------|-----------|-------------|
| `typescript-web` | `@putnami/typescript` | React SSR web app: file-routed pages, a shared layout, error and not-found boundaries, a loader/action pair, and one client-side island |
| `typescript-server` | `@putnami/typescript` | HTTP server with `@putnami/application`, file-based API routing with declared query/body input, and tests |
| `typescript-library` | `@putnami/typescript` | TypeScript library with a public entry point and a test that imports through it |
| `go-server` | `@putnami/go` | HTTP server on the Putnami Go framework (`app`, `http`, `config`, `logger`): typed port configuration, recovery/request-id/logging middleware, a JSON route, health, and a handler test |
| `go-library` | `@putnami/go` | Go library with an exported function and a test |
| `python-server` | `@putnami/python` | Experimental FastAPI server with uvicorn and tests; explicit opt-in, no Go/TypeScript parity |
| `python-library` | `@putnami/python` | Experimental Python library with importable module and tests; explicit opt-in, no Go/TypeScript parity |

A template is deliberately the smallest project that already works, not a
demonstration of the framework — that is what the samples are for. Each of these
seven is classified in the workspace support catalog (`putnami.support.json`):
the Go and TypeScript templates are `stable`, the Python ones `experimental` and
non-default. The reasoning behind what a default template promises is recorded
in `tooling/scaffold/doc/adr/0003-what-a-default-template-promises.md`.

```bash
# TypeScript
putnami projects create my-app --template typescript-web
putnami projects create my-api --template typescript-server
putnami projects create my-lib --template typescript-library

# Go
putnami projects create my-api --template go-server
putnami projects create my-lib --template go-library

# Experimental Python (explicit opt-in)
putnami deps add @putnami/python
putnami extensions install
putnami projects create my-api --template python-server
putnami projects create my-lib --template python-library
```

Every template produces a project you can build, test and lint immediately. The
`-server` and `-web` templates additionally declare a serve entry point, so they
can be started as well:

```bash
putnami serve my-app     # applications: typescript-web, typescript-server, go-server, python-server
putnami test my-lib      # every template
```

## Template file processing

Files ending in `.template` are processed with variable substitution and lose the suffix. Other files are copied as-is — a placeholder in one of them is **not** substituted and reaches the generated project as literal text.

Available variables:

| Variable | Description |
|----------|-------------|
| `<%= projectName %>` | The project name passed to `create` |
| `<%= projectPath %>` | Path to the project relative to workspace root |
| `<%= projectModule %>` | Normalized module name (e.g., `my-lib` becomes `my_lib` for Python) |
| `<%= putnamiVersion %>` | Current Putnami CLI version |
| `<%= workspaceRelativePath %>` | Relative path from project to workspace root |
| `<%= goFrameworkVersion %>` | Version to require for `go.putnami.dev/*` modules: the `@latest` version of `go.putnami.dev/app` that your Go module proxies serve. `create` asks for it only when a template uses the variable, and fails before it writes anything when no proxy answers |

Directory and file **names** support one substitution: the literal segment
`__module__` is replaced with the normalized module name, which is how the
Python templates produce `src/<module>/`.

Files that describe the template as a project *in the workspace it lives in* are
never staged into the archive, so a generated project does not inherit them:
`putnami.json`, `putnami.features.json`, `specs/`, `node_modules/`, and anything
whose name starts with a dot.

## Template discovery

Templates are discovered from three sources:

1. **Workspace projects** — any project containing a `putnami.template.json`
2. **Convention directories** — `<domain>/templates/*/` paths in the workspace
3. **Installed templates** — downloaded from the template registry

## Installing external templates

Configure installable templates in `putnami.workspace.json`:

```json
{
  "templates": [
    "typescript-library",
    "go-server:^1.0.0"
  ]
}
```

Then install:

```bash
putnami templates install
```

Or install as part of the combined workspace setup:

```bash
putnami install
```

Templates are downloaded and stored in `.putnami/templates/<name>/<version>/`. The unified lock file (`putnami.lock.json`) ensures deterministic installs.

## Managing templates

```bash
putnami templates list               # List configured and discovered templates
putnami templates update             # Update to latest compatible versions
putnami templates remove <name>      # Remove an installed template
```

Authoring a template uses the `dev template` group:

```bash
putnami dev template validate [path] # Validate a template manifest
putnami dev template test [path]     # Render the template and check the result
putnami dev template package [path]  # Package for distribution
```

## Publishing templates

To publish your own template, configure the `template-archives` publish channel in the template project's `putnami.json`:

```json
{
  "publish": ["template-archives"]
}
```

Then package and publish:

```bash
putnami package --projects my-template
putnami publish --projects my-template
```

Packaging is provided by the `@putnami/scaffold` extension, which activates on
`putnami.template.json`. It produces `<name>-<version>.tar.gz` with the template
manifest stamped to the packaged version, and records the `template-archives`
channel in the project's shared package metadata index for `publish` to read
back. `putnami dev template package` is the one-off equivalent for a template
that is not a workspace project.
