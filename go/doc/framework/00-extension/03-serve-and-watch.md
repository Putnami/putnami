# Serve & Watch

`serve` runs Go applications as servers. In development it watches files and restarts the process; in production it runs the built binary.

```bash
putnami serve .
putnami serve . --port 8080
```

## Entrypoint detection

When no entrypoint is provided, the extension resolves a runnable package in this order:

1. root `main.go`
2. a single `cmd/<name>` directory
3. preferred command directories such as `cmd/serve`, `cmd/server`, `cmd/<project-name>`, or `cmd/api`
4. an explicit error telling you to pass `--entrypoint`

## Options

| Option | Use |
|--------|-----|
| `--watch <boolean>` | Enable development watch mode |
| `--port <number>` | Set the `PORT` environment variable |
| `--killPort` | Kill the existing process using the port before starting |
| `--args <string>` | Pass arguments to the Go process |

## Watch behavior

Watch mode monitors Go files in the project and dependency projects. It ignores vendor directories, test files, and generated directories that should not restart the server.
