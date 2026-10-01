# Serve & Watch

`serve` starts a TypeScript development server and keeps generated output current while you work.

## Entrypoint resolution

The extension resolves the application entrypoint from:

1. `./serve` in `package.json` exports
2. `--entrypoint <path>`
3. a fast build fallback when no serve export exists

```bash
putnami serve .
putnami serve . --port 4000
putnami serve . --entrypoint src/server.ts
```

## Options

| Option | Use |
|--------|-----|
| `--entrypoint <path>` | Override the serve entrypoint |
| `--watch <boolean>` | Enable or disable watch mode |
| `--port <number>` | Choose the HTTP port |
| `--debug` | Forward `LOG_LEVEL=debug` to the application |
| `--no-services` | Disable multi-service discovery |
| `--inspect`, `--inspect-wait`, `--inspect-brk` | Enable Bun inspector modes |

## Multi-service mode

Declare runtime service dependencies in `package.json`:

```json
{
  "putnami": {
    "runsWith": ["catalogue-api", "profile-api"]
  }
}
```

Then run:

```bash
putnami serve web-app --watch
```

Putnami starts the web app and its declared services together. If `runsWith` is absent, serve walks the dependency graph for projects with `./serve` exports.

## Port conflicts

The serve job checks for port conflicts before starting. Conflicts are resolved interactively, or automatically in CI, and the selected port is saved to project config for future runs.
