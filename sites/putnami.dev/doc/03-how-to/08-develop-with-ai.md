# Develop with AI assistants

Use AI coding assistants like Claude Code, Codex, Cursor, or GitHub Copilot to build Putnami applications faster. When you initialize a workspace, Putnami inserts one short block into your assistant entrypoints (`AGENTS.md` and `CLAUDE.md`) that teaches tools your project's CLI commands and conventions. Everything else in those files is yours.

## What you get

`putnami init` writes two things for assistants at the workspace root:

- **`AGENTS.md` / `CLAUDE.md`** — one delimited Putnami block. A new `CLAUDE.md` is only `@AGENTS.md`; an existing file keeps every line outside the block, and an edited block is preserved, never overwritten
- **`.mcp.json`** — the `putnami` MCP server entry, so a new agent session reaches the Putnami tools with no manual step. `putnami install` and `putnami upgrade` add the entry too when it is missing

The block is short on purpose. It states:

- **CLI commands** — use `./putnamiw` when present, otherwise `putnami`; never npm, bun, go, pip, or cargo for build, test, lint, install, dependency, or generation work
- **Selection** — iterate with `--projects <a>,<b>`, and run the gate once with `--impacted --enforce-coverage` before declaring work complete
- **Discovery** — the first code-discovery step is a Putnami MCP call (`putnami.search`, `putnami.context`, `putnami.impact`), even when the agent already knows a keyword or a file name; file search is for literal text, or for when a tool answers stale or unavailable
- **Skills** — the skills and agents an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami
- **Your rules** — read `.agents/constraints.md` when it exists

`.agents/constraints.md` is a file *you* write when you have durable project rules to state; Putnami only reads it. A workspace that kept its rules in `.AI/constraints.md` moves the file with `git mv .AI/constraints.md .agents/constraints.md`.

Framework recipes are not copied into your repository. An MCP-aware assistant reads them live from the `putnami://extension-guidance/AI.md` resources, which carry the exact bytes of the extension versions your lock pins.

## Context, tools, and workflows are separate layers

Putnami onboarding has three independent layers:

| Layer | Drives |
|---|---|
| the Putnami block in `AGENTS.md` / `CLAUDE.md` | the commands, selection rules, and discovery routing the assistant follows; plus your own `.agents/constraints.md` when you write one |
| `.mcp.json`, written by `putnami init`, `install` and `upgrade` when the entry is missing | read-only structured workspace facts for assistants that support MCP |
| the agent content of `@putnami/contributor` | repeatable workflows such as planning, implementing, checking, and reviewing |

You need a Putnami workspace (preferably with its pinned `./putnamiw` wrapper)
and a coding assistant that can discover the generated entrypoint or installed
skills. You do **not** need an account with a source host, work-item tracker, or
review service to use the core workflows. Connectors for those services are
optional and are installed only when you want that automation.

An MCP-aware assistant should use Putnami's read-only structural tools before
enumerating files. A non-MCP assistant uses `./putnamiw` (or `putnami` when the
workspace has no wrapper) and focused file reads. In both cases the pinned
Putnami command remains authoritative for build, test, lint, validation,
dependency, and install workflows.

| Setup | Workspace discovery | Default workflow result |
|---|---|---|
| MCP-aware + core only | local Putnami MCP tools | local plan/change/check/review |
| Non-MCP + core only | wrapper/CLI fallback | the same local result |
| Core + optional integrations | Putnami tools plus separately configured host capabilities | local by default; an external write only when explicitly requested |

Source hosting and work-item tracking are independent choices. A team may use
either, combine services from different vendors, or configure neither. The core
never infers one from the other.

## Install and update agent workflows

Agent workflows are the agent content of an extension. A workspace declares the
extension and opts into its content explicitly:

```json
{
  "extensions": ["@putnami/contributor"],
  "agentArtifacts": ["extension:@putnami/contributor"]
}
```

Declaring an extension without the opt-in activates none of its content.

`@putnami/contributor` is published on the `canary` channel first, and on
`stable` and `latest` at the next promotion. Every built-in starter writes
both declarations. Until the channel your workspace resolves serves the
extension, `putnami init` finishes the workspace without workflow files and
names `putnami install`, and `putnami install` fails at its extensions phase.
To install the rest of a new workspace now, remove both entries, or resolve
`canary`, and add them back once your channel serves it. A workspace that
declares the extension by path installs its content today.

Reading agent content needs a CLI that speaks extension contract 5, the
release that introduces it. An earlier CLI refuses a content-bearing
extension with "requires a newer putnami" and fails `putnami install` in a
workspace that opted in, so upgrade the CLI before adding the opt-in.

Commit the declarations, the extension's lock pin, and the materialized workflow
files. A clone then runs `./putnamiw install`, which reads the content out of
the exact extension release the lock pins and does not resolve or rewrite the
lock. Use `./putnamiw upgrade --extensions` to move the extension; its content
follows in the same command.

Removing the `extension:` entry retires the content on the next upgrade: files
you did not change are removed, and a file you edited is kept and reported.
The content owns only the manifest paths and byte digests it recorded; an
unrecorded or locally modified file is preserved as a collision.

A workspace that still declares workflows as a separate artifact, such as
`@putnami/agent-workflows`, is refused by every command, which names the one
that moves it:

```bash
./putnamiw migrate agent-content @putnami/contributor           # review
./putnamiw migrate agent-content @putnami/contributor --apply   # move
```

## Reach the MCP server

An agent IDE discovers the server through a `.mcp.json` file at the workspace root. `putnami init`, `putnami install` and `putnami upgrade` add the `putnami` entry when it is missing. They merge: other servers and unknown keys are preserved, a `putnami` entry you changed is kept as you wrote it, and a file that does not parse is left untouched with a warning.

To write or repair the entry explicitly, run:

```bash
putnami mcp install
```

It merges the same way, and it also rewrites a diverged `putnami` entry.

When an agent session starts, the server brings the managed skills to the extension release your lock pins, so a worktree that switched branches does not keep the skills of its old lock. It copies them from the installed extension only; when you edited a managed file, it logs a warning and starts anyway, and `putnami install` finishes the job.

## Polyglot workspaces

If you add extensions after the initial setup, regenerate the guidance block and the managed skills:

```bash
putnami extensions install @putnami/go
putnami context generate
```

Language recipes do not land in your repository: each extension's `AI.md` is served live by the MCP server at the version your lock pins.

## Orient in a project with agent context

When an assistant (Claude, Codex, or any MCP-aware tool) needs to understand a single project — where its composition roots are, which contracts and capabilities it exposes, which source is worth reading first, and how it is tested — Putnami can hand it those framework-owned facts directly instead of making it enumerate the repository.

### `putnami context pack`

```bash
putnami context pack --project <project-id>
```

`context pack` aggregates one project's facts **by reference** into `<project>/.gen/agent-context.json`: identity and dependency graph, composition roots (application main, describe entrypoint), capability/contract/infra/migration references (path + digest), representative source **ranges** (never file content), a tests section (conformance packs or a machine-readable absence reason), adjacent docs, and a config-schema reference. Omit `--project` to pack every selected project.

The artifact is **ephemeral**: it lives under `.gen/` (gitignored, never committed) because it embeds content digests and the workspace revision and would churn on every commit. Regenerate it on demand — do not check it in.

### `putnami context pack --check`

```bash
putnami context pack --check
```

`--check` is a freshness gate: it verifies the on-disk artifact matches what the aggregator would produce now and **exits 2 on drift** (for example after you change a referenced source or a manifest) without rewriting anything. Use it in a pre-flight or CI step to catch a stale context.

### The `agent_context` MCP tool

The Putnami MCP server exposes a read-only `agent_context` tool. An agent calls it to get a single project's identity, composition roots, contracts, representative sources, and tests in **one structured response** — including on-disk freshness — instead of listing directories and reading files one by one. It complements `describe_project`: `describe_project` answers "what commands/tasks/dependencies does this project have", while `agent_context` answers "how is this project composed and where should I read first".

The MCP server advertises these tools at initialization, so an agent connected to your workspace already knows they exist.

## Say what you want built, once

Framework facts tell an assistant how your project is composed; they never say what a change is *for*. That is what a spec holds — intended outcomes, non-goals, and the sentences your team agreed to — in one small, strictly validated file per feature. See [Write a feature spec](/docs/how-to/write-a-feature-spec).

## Prompt recipes

Here are prompts that work well with AI assistants in a Putnami project. The assistant reads the framework patterns live from the MCP server's extension-guidance resources.

### Web applications (typescript-web)

**Pages and routing:**

- "Add a page at /dashboard that shows a welcome message"
- "Add a page at /users/[id] that displays user details"
- "Add a layout for the /admin section with a sidebar navigation"

**Data loading:**

- "Add a loader to the dashboard page that fetches stats from /api/stats"
- "Add a loader to /users/[id] that fetches user data by ID"

**Forms and actions:**

- "Add a form on /settings that lets users update their name and email"
- "Add a contact form at /contact with name, email, and message fields"

**API endpoints:**

- "Add a /api/users REST API with GET (list) and POST (create) endpoints"
- "Add a GET /api/users/[id] endpoint with UUID validation"
- "Add pagination with page and limit query params to GET /api/tasks"

### API services (typescript-server)

- "Add CRUD endpoints for a tasks resource with UUID ids"
- "Add request validation to the POST /api/tasks endpoint — require name (string) and priority (int)"
- "Add a GET /api/health endpoint that returns the server version"
- "Add an endpoint that accepts file uploads"

### Database and persistence

- "Connect to PostgreSQL and create a users table with id, name, and email"
- "Add a tasks table with id, title, done status, and created date"
- "Create a repository for the users table with find-by-email support"

### Authentication

- "Add OAuth2 authentication and protect the /dashboard route"
- "Add session-based auth with a login page"

### Go services (go-server)

- "Add a /health endpoint that returns JSON"
- "Add CRUD handlers for a tasks resource"
- "Add request logging middleware"

### Python services (python-server)

`python-server` is experimental and explicit opt-in. It is not a default path
and has no Go or TypeScript parity promise; first run
`putnami deps add @putnami/python`, then create it with
`putnami projects create <name> --template python-server`.

- "Add a /tasks endpoint with GET and POST methods"
- "Add a Pydantic model for task validation"

## Customizing shared AI rules

Create `.agents/constraints.md` to add project-specific rules. The Putnami block already tells assistants to read it when it exists, and Putnami never writes or overwrites it, so the file stays entirely yours.

Good things to add:

- **Business rules** — "Tasks belong to projects. A user can only see tasks in their projects."
- **Naming conventions** — "Use camelCase for TypeScript, snake_case for database columns."
- **Architecture decisions** — "This app uses a modular architecture. Each module has its own api/, web/, and data/ directories."
- **External dependencies** — "We use Stripe for payments. The API key is in the STRIPE_API_KEY env var."

## Tips for effective prompting

1. **Be specific about resource names** — "Add a users endpoint" works better than "add an endpoint"
2. **Mention validation requirements** — "name is required, email must be valid" helps the AI use the right schema types
3. **Reference sample projects** — For complex patterns, point the AI to `typescript/samples/13-fullstack-app` or other samples
4. **Start simple, iterate** — "Add a page at /dashboard" then "Add a loader that fetches user stats" works better than one giant prompt
5. **Use `putnami serve` to verify** — After changes, run the app and check the result
