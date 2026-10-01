# Python Extension (experimental)

`@putnami/python` provides an explicit opt-in project lifecycle inside a
Putnami workspace: dependency sync, test, lint, serve, Docker packaging, and
template support. It is not a default path and makes no Go or TypeScript parity
promise.

Python is extension-first. The extension coordinates the available Python
workflow while keeping Python tools native; it does not provide a Python
framework package family.

## Extension phases

| Phase | Start here | What it explains |
|-------|------------|------------------|
| Setup | [Detection & dependencies](/docs/frameworks/python/extension/detection-and-dependencies) | How Python projects are found and how uv is used |
| Feedback loop | [Test & lint](/docs/frameworks/python/extension/test-and-lint) | pytest, Ruff, and common options |
| Local runtime | [Serve & watch](/docs/frameworks/python/extension/serve-and-watch) | Entrypoints, ports, and server execution |
| Release shape | [Docker & templates](/docs/frameworks/python/extension/docker-and-templates) | Docker output and project templates |

## Explicitly enable the extension

```bash
putnami deps add @putnami/python
putnami deps install
```

The extension is discovered only after this explicit registration. Do not add
it to a workspace merely to follow the default path; choose it for an
experiment.

## External tools

- [Python](https://www.python.org)
- [uv](https://astral.sh/uv)
- [Ruff](https://docs.astral.sh/ruff)
- [pytest](https://pytest.org)

## Daily loop

```bash
putnami test api
putnami serve api
putnami lint,test,build --impacted
```
