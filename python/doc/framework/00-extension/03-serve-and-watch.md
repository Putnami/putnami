# Serve & Watch

`serve` runs a Python entrypoint as a server through uv.

```bash
putnami serve .
putnami serve . --entrypoint src/server.py
```

## Entrypoint selection

The extension uses:

1. `project.main` when configured
2. `src/main.py` by default
3. `--entrypoint <path>` when explicitly provided

## Options

| Option | Use |
|--------|-----|
| `--entrypoint <path>` | Override the Python entrypoint |
| `--port <number>` | Set the server port |

For FastAPI projects created from `python-server`, the template provides the expected server shape and test setup.
