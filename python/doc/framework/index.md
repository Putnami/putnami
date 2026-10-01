# Python (experimental)

Python is an **experimental, explicit opt-in** Putnami workspace surface for
small services, libraries, data workloads, and automation. It is not a default
project path and has no compatibility or feature-parity promise with Go or
TypeScript.

> Status: extension-first. Python is limited to templates, dependency sync,
> lint, test, serve, and Docker packaging. Putnami does not ship a Python
> framework package family.

## What is ready today

| Capability | Where to start | Why it matters |
|------------|----------------|----------------|
| First experiment | [Getting Started](/docs/frameworks/python/getting-started) | Explicitly create a service or library experiment through Putnami |
| Project lifecycle | [Extension phases](/docs/frameworks/python/extension) | Detection, dependency sync, test, lint, serve, Docker, uv, Ruff, pytest |
| New services and libraries | [Templates](/docs/tooling-&-workspace/templates) | `python-server` and `python-library` start with workspace conventions |
| Workspace orchestration | [Tooling & Workspace](/docs/tooling-&-workspace) | Explicitly configured Python projects participate in graph selection and impacted jobs |
| Automation output | [CLI](/docs/tooling-&-workspace/cli) | Deterministic command output for CI and agents |
| Fast feedback loops | [Jobs & caching](/docs/tooling-&-workspace/jobs-&-caching) | Cache-aware test/lint/serve behavior inside the shared runner |

## Good fits

Use experimental Python inside Putnami when you deliberately want:

- a FastAPI service created from the `python-server` template
- an importable package created from the `python-library` template
- data, ML, or automation code in the same repo as product services
- CI and agent workflows that call `putnami test`, `putnami lint`, and `putnami serve` instead of bespoke scripts
- a small Docker-ready experiment without hand-maintaining the initial project shape

## How Python fits the workspace

Explicitly configured Python projects are workspace nodes. They can be selected,
tested, linted, and served through Putnami commands, while Python-specific
behavior stays delegated to uv, Ruff, pytest, and the generated project shape.
This does not imply that Python is default, production-ready, or equivalent to
the Go and TypeScript surfaces.

```bash
putnami deps add @putnami/python
putnami extensions install
putnami projects create recommender --template python-server
putnami deps install
putnami test recommender
putnami serve recommender
```

## Read next

1. Read [Getting Started](/docs/frameworks/python/getting-started) to create an experimental Python service or library.
2. Use [How To / Guides](/docs/how-to) for concrete workspace tasks.
3. Read [Extension phases](/docs/frameworks/python/extension) for the exact jobs and options.
4. Read [Templates](/docs/tooling-&-workspace/templates) before creating a new Python project.
5. Read [Workspace](/docs/tooling-&-workspace/workspace) if you need to understand how Python projects participate in impacted work.

If you need a richer web or backend framework today, start with [TypeScript](/docs/frameworks/typescript) or [Go](/docs/frameworks/go), then use Python where it adds the most leverage.
