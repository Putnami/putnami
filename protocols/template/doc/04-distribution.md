# Distribution

Templates are distributed as platform-independent archives that can be published
to a registry and installed into a fresh workspace.

## Lifecycle

```
define → test → package → publish → install → create
```

1. **Define** — template files plus a `putnami.template.json` manifest;
2. **Test** — `putnami dev template test` renders and builds the template;
3. **Package** — `putnami dev template package` creates a `.tar.gz` archive;
4. **Publish** — `putnami publish` uploads the archive on the
   `template-archives` channel;
5. **Install** — `putnami templates install` downloads, verifies and extracts
   the archive;
6. **Create** — `putnami projects create` scaffolds a project from it.

## Packaging

```bash
putnami dev template package [path] [--version <ver>] [--stable] [--output <dir>]
```

The archive is a single platform-independent `<name>-<version>.tar.gz`
containing the staged template directory: the manifest with the resolved version
stamped into it, every template and static file, and `README.md` / `LICENSE.md`
when present.

The version is resolved as `--version`, else the manifest's `version`, else
`0.0.0`.

| Flag | Purpose |
|------|---------|
| `--version <ver>` | Version stamped into the packaged manifest |
| `--stable` | Record the archive as a stable release in the metadata |
| `--output <dir>` | Override the archive output directory |

Default output, relative to the template directory:

```
<template>/.putnami/out/package/
├── metadata.json                  # {version, artifact, channels, stable, template}
└── archives/
    └── <name>-<version>.tar.gz
```

The command prints the archive path, the resolved version, the SHA-256
integrity, and the metadata path.

### Template project config

A template becomes publishable by declaring a `putnami.json` next to its
manifest:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "name": "my-template",
  "type": "template",
  "extensions": ["@putnami/scaffold"],
  "publish": ["template-archives"],
  "options": {
    "publish": { "archives": true }
  }
}
```

## Publishing

```bash
putnami publish --projects my-template
```

Templates reuse the same archive publishing path as extensions: the publish step
reads `metadata.json` from the package output, uploads the archive, and creates
the tag manifests that version resolution reads.

## Installation

```bash
putnami templates install              # install everything the workspace declares
putnami templates install <name>       # install one template
putnami templates install --latest     # ignore the lock and resolve latest
```

The resolver base URL defaults to `https://put.putnami.dev` and is overridden by
`PUTNAMI_REGISTRY_URL`. Archives are fetched from
`<resolver>/<namespace>/<package>/download?channel=<constraint>`.

Installed artifacts land in the shared `.putnami/bin` layout, not in a
template-specific directory:

```
.putnami/bin/
├── templates/
│   └── go-library            → ../artifacts/templates/go-library@1.2.0/
└── artifacts/
    └── templates/
        └── go-library@1.2.0/
```

`templates/<name>` is a stable symlink; verified installs may point it at the
machine-global content-addressed artifact store shared by every worktree.

### Lock file

`putnami.lock.json` at the workspace root records exact versions and integrity
hashes for reproducible installs. Template entries live in the `templates`
section and use the same shape as extension entries:

```json
{
  "version": 4,
  "extensions": {},
  "templates": {
    "go-library": {
      "version": "1.2.0",
      "integrities": {
        "linux/x64": "sha256-…"
      },
      "manifestHash": "sha256-…",
      "source": "https://put.putnami.dev/putnami/go-library/download?channel=1.2.0"
    }
  }
}
```

The lock format is versioned separately by
[`protocols/workspace`](../../workspace/README.md): the accepted read window is
v2–v4, and new locks are authored as v4. Older entries carrying a single
`integrity` field instead of the per-platform `integrities` map remain readable.

### Workspace configuration

Declare installable templates in `putnami.workspace.json`:

```json
{
  "templates": [
    "go-library",
    "typescript-server:^1.0.0"
  ]
}
```

An entry is `<name>` or `<name>:<version constraint>`.

## Command summary

```bash
putnami templates install [name]       # install from workspace configuration
putnami templates list                 # list configured and discovered templates
putnami templates update [name]        # update to the latest compatible version
putnami templates remove <name>        # remove an installed template

putnami dev template validate [path]   # validate a manifest
putnami dev template test [path]       # render, build and test a template
putnami dev template package [path]    # package for distribution
```
