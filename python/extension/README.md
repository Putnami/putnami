# @putnami/python

`@putnami/python` is an **experimental** integration for explicitly opted-in
Python projects. It is not a default Putnami path and makes no compatibility or
feature-parity promise with Go or TypeScript. It provides workspace jobs around
uv, Ruff, pytest, and a configured Python entrypoint; it is not a Python
framework.

## Features

- **Lint**: format and check Python code with [Ruff](https://docs.astral.sh/ruff/)
- **Test**: run pytest with auto-discovery (`test_*.py` / `*_test.py`)
- **Serve**: run a Python server via UV with optional hot-reload
- **Run**: run a workload once for the host and forward its exit code
- **UV workspace sync**: auto-discover Python packages and manage root `pyproject.toml`
- **Templates**: scaffold Python servers and libraries in one command

## Requirements and opt-in

- Python 3.11+
- [UV](https://astral.sh/uv/) in PATH
- An explicit workspace opt-in: `putnami deps add @putnami/python`, followed
  by `putnami extensions install`

The experimental Python templates are not selected by workspace initialization
or by a default project command. Choose one deliberately:

## Experimental quick start

### Scaffold a server

```bash
putnami projects create api-server --template python-server
putnami workspace-install --force
putnami serve api-server
```

### Scaffold a library

```bash
putnami projects create py-lib --template python-library
putnami workspace-install --force
putnami test py-lib
```

### Lint and test an existing project

```bash
putnami lint api-server
putnami test api-server
```

## Jobs Reference

| Job | Command | Purpose | Docs |
|-----|---------|---------|------|
| **Lint** | `putnami lint <project>` | Format + check with Ruff | [doc/lint.md](./doc/lint.md) |
| **Test** | `putnami test <project>` | Run pytest | [doc/test.md](./doc/test.md) |
| **Serve** | `putnami serve <project>` | Run a Python server via UV | [doc/serve.md](./doc/serve.md) |
| **Run** | `putnami run <project>` | Run a workload once and forward its exit code | [doc/run.md](./doc/run.md) |
| **Workspace Install** | `putnami workspace-install` | Sync UV workspace + lock | [doc/workspace-install.md](./doc/workspace-install.md) |

## Support and contract

The SDD owner is `python`. `@putnami/python` is classified `experimental`,
`default: false`, and `parity: unsupported` in the workspace
[support catalog](../../putnami.support.json). Its user-facing behavior is
defined by the [experimental workspace integration specification](specs/experimental-workspace-integration.json)
and the [experimental Python surface ADR](doc/adr/0001-experimental-python-surface.md).
The extension stays `modeled` because its current build and test jobs do not
produce exact Feature Evidence; tests protect behavior but are not manually
promoted to evidence.

## Documentation

- [Overview](./doc/overview.md)
- [Getting Started](./doc/getting-started.md)
- [Configuration](./doc/configuration.md)
- [Jobs](./doc/jobs.md)
- [Templates](./doc/05-templates.md)
- [Lint](./doc/lint.md)
- [Test](./doc/test.md)
- [Serve](./doc/serve.md)
- [Run](./doc/run.md)
- [Workspace Install](./doc/workspace-install.md)

## License

[FSL-1.1-MIT](../../LICENSE.md)
