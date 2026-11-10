# Monorepo tools

A monorepo tool integration puts Putnami's runner, impact selection and
verdict on a repository that Bazel, moon, Nx, Pants, Rush or Turborepo already
describes, without rewriting its build. `putnami init --from <tool>` reads the
tool's packages and their dependencies and turns each package into a Putnami
project; `putnami lint,test,build --impacted` then runs what a change affects.

Each integration adopts the tool in one of two modes:

- **Delegate**: the tool keeps running every task and keeps its own cache.
  Putnami runs it on each affected package, reads its reports, and caches
  nothing for those tasks.
- **Import**: Putnami's runner and cache take over the tasks a Putnami
  extension can map, and every other task keeps running through the tool.

| Tool | Modes | Reference |
|---|---|---|
| Bazel | Delegate | [Run a Bazel workspace with Putnami](/docs/monorepo-tools/bazel) |
| moon | Import or delegate | [Run a moon workspace with Putnami](/docs/monorepo-tools/moon) |
| Nx | Import or delegate | [Run a Nx repository with Putnami](/docs/monorepo-tools/nx) |
| Pants | Delegate | [Run a Pants repository with Putnami](/docs/monorepo-tools/pants) |
| Rush | Import or delegate | [Adopt a Rush repository](/docs/monorepo-tools/rush) |
| Turborepo | Import or delegate | [Run a Turborepo repository with Putnami](/docs/monorepo-tools/turbo) |

Each reference is the integration's own documentation, published as written
next to its code. To adopt a repository step by step, follow
[Adopt Putnami on an existing monorepo](/docs/how-to/adopt-putnami-on-an-existing-monorepo).
Every language Putnami runs, and the support status of each extension, is on
[Languages and monorepo tools](/docs/languages).
