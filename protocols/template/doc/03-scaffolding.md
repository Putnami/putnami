# Scaffolding

Templates are used by `putnami projects create` to scaffold new projects in a
workspace.

## Creating a Project

```bash
putnami projects create <name> --template <template> [--path <path>] [--force]
```

| Flag | Purpose |
|------|---------|
| `--template <template>` | The template to scaffold from |
| `--path <path>` | Workspace-relative project path (defaults to the project name) |
| `--force` | Overwrite an existing project directory |

This:

1. Discovers the template from workspace projects, domain templates, or
   installed templates;
2. Creates the project directory;
3. Copies static files verbatim;
4. Renders `.template` files with variable substitution and strips the suffix
   (see [`02-variables.md`](02-variables.md));
5. Ensures the manifest's `extension` is registered in the project config;
6. Adds `workspaceDevDependencies` to the workspace root, if the manifest
   declares any.

## Template Discovery

Templates are discovered from three sources, in priority order:

1. **Workspace projects** — a project directory containing
   `putnami.template.json`;
2. **Domain templates** — `<domain>/templates/*/putnami.template.json`, by
   convention (this repository's `go/templates/`, `typescript/templates/`,
   `python/templates/`);
3. **Installed templates** — artifacts under `.putnami/bin/templates/` resolved
   through `putnami.lock.json`.

The first source to claim a name wins. When the lock file cannot be read, the
installed templates resolve no version; discovery continues and warns once per
workspace rather than failing.

## Testing Templates

Check that a template still produces a project that builds:

```bash
putnami dev template test [path] [--skip-build] [--keep]
```

This:

1. Creates a temporary workspace directory;
2. Renders the template into it using the default test variables, with any
   `testVariables` overrides from the manifest applied;
3. Writes a minimal `putnami.workspace.json`, plus a root `package.json`
   carrying `workspaceDevDependencies` when the manifest declares them;
4. Registers the manifest's `extension` in the rendered project config;
5. Verifies the rendered project has a `putnami.json` — a template that produces
   none is broken;
6. Runs `putnami install`, then `putnami build,test --projects <projectName>
   --no-cache --verbose` in the temporary workspace;
7. Reports pass or fail and removes the temporary workspace.

| Flag | Purpose |
|------|---------|
| `--skip-build` | Stop after rendering and structure validation (steps 1–5) |
| `--keep` | Keep the temporary workspace and print its path |

### Test Variables

The default render values are listed in [`02-variables.md`](02-variables.md). A
template overrides `projectName` and/or `projectModule` through `testVariables`
in `putnami.template.json`:

```json
{
  "name": "my-template",
  "description": "…",
  "testVariables": {
    "projectName": "my-custom-test-name"
  }
}
```

Other keys are accepted by the manifest schema but ignored by the test command.

## Validating Templates

```bash
putnami dev template validate [path]
```

See [`01-manifest.md`](01-manifest.md) for exactly what it checks.
