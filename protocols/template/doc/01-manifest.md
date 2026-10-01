# Template Manifest

Every Putnami template is defined by a `putnami.template.json` manifest file at the template root. Templates scaffold new projects with a predefined structure, configuration, and extension bindings.

## Minimal Manifest

The smallest valid template manifest:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-template.json",
  "name": "my-library",
  "description": "A library project template"
}
```

## Top-Level Fields

| Field | Required | Description |
|-------|----------|-------------|
| `$schema` | no | JSON Schema reference for editor support |
| `name` | **yes** | Template identifier (e.g., `typescript-library`, `go-server`) |
| `description` | **yes** | Human-readable description |
| `version` | no | Template version (semver) |
| `extension` | no | Extension to activate for projects created from this template |
| `workspaceDevDependencies` | no | Dev dependencies to add to the workspace root |
| `testVariables` | no | Render-variable overrides for `putnami dev template test` (`projectName`, `projectModule`) |

Unknown fields are rejected by both the strict parser and the schema.

## Template Directory Structure

A template is a directory containing:

```
my-template/
├── putnami.template.json     # Manifest (required)
├── putnami.json.template     # Project config template
├── src/
│   └── index.ts.template     # Source file templates
├── test/
│   └── index.test.ts.template
└── tsconfig.json              # Static files (copied as-is)
```

Files with `.template` extension are processed with variable substitution. Files without the extension are copied verbatim.

The manifest itself is skipped during rendering: it is template metadata, not
project content, so it never lands in the created project.

## Validation

Validate a template manifest:

```bash
putnami dev template validate [path]
```

`[path]` may be the manifest, or a directory containing one; with no argument
the current directory is used. The command:

- strict-parses the manifest (unknown fields are errors) and validates that
  `name` and `description` are present, through this protocol package;
- reports each diagnostic as `<manifest path>: [code] field: message`;
- reports how many `.template` files the directory contains — zero is a notice,
  not a failure.

It does **not** check that the referenced `extension` exists, and it does not
inspect the contents of `.template` files.

## Schema

The formal schema is in [`schemas/template.json`](../schemas/template.json),
published as `https://putnami.dev/schemas/putnami-template.json`. It constrains
`name` to `^(@[a-z0-9-]+/[a-z0-9-]+|[a-z0-9][a-z0-9-]*)$`; the Go validator only
requires that `name` be non-empty, so a schema-aware editor rejects a malformed
name that `dev template validate` accepts.
