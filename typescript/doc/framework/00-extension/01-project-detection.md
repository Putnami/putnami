# Project Detection

The TypeScript extension runs only in project context. It is responsible for TypeScript projects that live inside the Putnami workspace graph: web apps, API services, libraries, and extension packages.

## What activates it

The extension is available when the workspace depends on `@putnami/typescript`. Project templates wire the project metadata automatically:

```bash
putnami projects create web --template typescript-web
putnami projects create api --template typescript-server
putnami projects create ui --template typescript-library
```

For existing projects, add the extension at the workspace level:

```bash
putnami deps add @putnami/typescript
putnami deps install
```

## Project shapes

| Template | Extension behavior |
|----------|--------------------|
| `typescript-web` | Build React SSR routes, generated assets, serve entrypoint, and deployable runtime output |
| `typescript-server` | Build API handlers, application runtime, tests, and server entrypoint |
| `typescript-library` | Build package exports, declaration files, and test output |

## Entrypoint signals

The extension reads package metadata to understand how a project should run:

- `exports` controls package entrypoints
- `./serve` identifies a serve entrypoint when present
- `bin` entries and `build.compile` identify executable outputs
- project config can add generated assets, publish channels, and compile targets

## Scope boundary

The extension does not replace workspace orchestration. Putnami still decides project order, impacted projects, cache reuse, and cross-project dependencies. The extension provides the TypeScript-specific jobs that run inside that orchestration.
