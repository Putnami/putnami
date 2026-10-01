# Generate Phase

`generate` runs before TypeScript build and test jobs. It prepares generated source, exports, and static assets so downstream phases see a complete project.

## What generate does

Key steps:

- discover pre-build hooks from plugin dependencies
- run hooks in dependency order
- write generated source and export metadata to `.gen/`
- copy configured generated assets into the project output

```bash
putnami generate .
putnami generate . --clear
```

## Options

| Option | Use |
|--------|-----|
| `--verbose` | Show detailed generation output |
| `--clear` | Remove existing generated artifacts before running |

## Pre-build hooks

Dependencies can expose pre-build hooks through a `./pre-build` export. The runner passes:

- `--context <path>` with the job context
- `--result <path>` where the hook writes its JSON result

Example result:

```json
{
  "exports": {
    "./generated": "./.gen/main.ts"
  },
  "assets": [
    { "src": ".gen/schema.json", "dest": "schema.json" }
  ]
}
```

## Create a hook

1. Export `preBuild` from `src/pre-build/index.ts`.
2. Provide a command and optional arguments.
3. Write the result JSON to the path received in `--result`.
4. Add the export to `package.json`.

```json
{
  "exports": {
    "./pre-build": "./src/pre-build/index.ts"
  }
}
```
