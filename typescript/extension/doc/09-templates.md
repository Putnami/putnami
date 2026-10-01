# Templates

The TypeScript extension provides three project templates for scaffolding new projects in a Putnami workspace.

## Overview

- Scaffold production-ready project structures with a single command
- Three templates for different use cases: library, API server, and web application
- Templates include source files, test files, and TypeScript configuration
- Template variables (`<%= projectName %>`, `<%= putnamiVersion %>`) are interpolated at creation time

## Usage

```bash
putnami projects create <name> --template <template-name>
```

## Available Templates

### `typescript-library`

A reusable TypeScript library package with exports.

```bash
putnami projects create my-lib --template typescript-library
```

**Generated structure:**

```text
my-lib/
├── src/
│   ├── index.ts              # Library entry point
│   └── hello.ts              # Example module
├── test/
│   └── hello.test.ts         # Example test
├── tsconfig.json
├── putnami.json
└── package.json
```

**package.json:** Sets `main: "src/index.ts"`. No external dependencies.

**Use case:** Shared utilities, domain models, or any reusable code consumed by other workspace projects via `workspace:*` dependencies.

### `typescript-server`

An HTTP API server built on `@putnami/application`.

```bash
putnami projects create my-api --template typescript-server
```

**Generated structure:**

```text
my-api/
├── src/
│   ├── main.ts               # Application bootstrap
│   ├── serve.ts              # Server entry point (./serve export)
│   └── api/
│       ├── get.ts            # GET route handler
│       └── post.ts           # POST route handler
├── test/
│   └── api.test.ts           # API integration test
├── tsconfig.json
├── .putnamirc.json
└── package.json
```

**package.json:** Exports `./serve` pointing to `src/serve.ts`. Depends on `@putnami/application`.

**Use case:** JSON APIs, backend services, microservices.

### `typescript-web`

A React web application with server-side rendering (SSR) built on `@putnami/web`.

```bash
putnami projects create my-app --template typescript-web
```

**Generated structure:**

```text
my-app/
├── src/
│   ├── main.ts               # Application bootstrap
│   ├── app/
│   │   ├── layout.tsx        # Root layout
│   │   ├── page.tsx          # Home page
│   │   ├── about/
│   │   │   └── page.tsx      # About page
│   │   ├── guestbook/
│   │   │   ├── page.tsx      # Guestbook page
│   │   │   ├── loader.ts     # Data loader
│   │   │   └── action.ts     # Form action
│   │   ├── error.tsx         # Error boundary
│   │   └── not-found.tsx     # 404 page
│   └── components/
│       └── counter.tsx       # Interactive client component
├── test/
│   └── app.test.ts           # Application test
├── tsconfig.json
├── .putnamirc.json
└── package.json
```

**package.json:** Depends on `@putnami/application`, `@putnami/web`, `@putnami/ui`, `react`, and `react-dom`.

**Use case:** Full-stack web applications with React SSR, file-based routing, loaders, and actions.

## Boundaries

- **Scope**: Scaffolding new project directories with starter files and configuration
- **Out of scope**: Updating existing projects, migrating between templates
- **Dependencies**: Requires the `putnami projects create` command from the CLI
- **Extension points**: None. Templates are static file trees with variable interpolation.
