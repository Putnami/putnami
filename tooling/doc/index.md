# Tooling & Workspace

Tooling is Putnami's control plane inside your repository. It discovers projects, builds the dependency graph, runs jobs, manages templates, and gives humans and automation one command surface.

> The promise: if the workspace can explain what changed, Putnami can run only the work that matters.

## Start with the problem you have

| You want to... | Read first | What you get |
|----------------|------------|--------------|
| Understand the repo model | [Workspace](/docs/tooling-&-workspace/workspace) | Projects, scopes, graph edges, and the `--impacted` model |
| Use Putnami day to day | [CLI](/docs/tooling-&-workspace/cli) | Commands, flags, aliases, JSONL output, shell completion |
| Make feedback loops fast | [Jobs & caching](/docs/tooling-&-workspace/jobs-&-caching) | Job orchestration, cache keys, dependencies, watch mode |
| Add language or platform support | [Extensions](/docs/tooling-&-workspace/extensions) | Discovery, extension manifests, JSONL protocol, custom jobs |
| Start a project correctly | [Templates](/docs/tooling-&-workspace/templates) | Scaffolded services, libraries, and reusable project shapes |
| Debug failed automation | [Error handling](/docs/tooling-&-workspace/error-handling) | Diagnostics, retries, exit codes, machine-readable failures |
| Generate clients from a provider | [Generated clients](/docs/tooling-&-workspace/generated-clients) | Workspace generate/sync/check, the drift guard, strict diagnostics, migration |

## The mental model

Putnami tooling is three layers working together:

1. **Workspace graph** - the source of truth for projects, dependencies, scopes, and impacted work.
2. **Job runner** - the execution engine for `build`, `test`, `lint`, `serve`, `publish`, and compound commands.
3. **Extension protocol** - the pluggable boundary where TypeScript, Go, Python, CI, and custom tools add capabilities.

This is why the same command can work locally, in CI, and inside an agent session:

```bash
putnami lint,test,build --impacted
putnami serve my-app
putnami projects create api --template go-server
```

## What belongs in this surface

Use Tooling & Workspace when the question is about how the repository operates:

- how projects are discovered and selected
- how impacted work is calculated
- how command output is structured for humans and machines
- how cache keys, inputs, and outputs are defined
- how templates produce consistent project shapes
- how extensions make Putnami polyglot without turning the CLI into a language-specific tool
- how a provider declaration becomes generated clients in every language, and how the gate keeps them honest

If the question is about how to write an application, jump to [TypeScript](/docs/frameworks/typescript), [Go](/docs/frameworks/go), or [Python](/docs/frameworks/python). Publishing now lives in each language extension; branch environments are an @putnami/cloud concern.
