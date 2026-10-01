---
name: plan
description: Explore the codebase and produce a structured implementation plan; create a task only when explicitly requested
model: claude-opus-5-5[1m]
allowed-tools: Bash, Read, Grep, Glob, TodoWrite, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: <feature description> [--scope <target>] [--label <label>] [--task] [--dry-run]
---

# Plan

Explore the codebase to understand scope and impact, then produce a structured
implementation plan. Planning is read-only by default. Create or update a
task only when the user explicitly requests that external action.

## Portable host contract

This is the canonical workflow for both Claude Code and Codex. Treat `/name`
references as logical skill invocations (`/name` on Claude Code, `$name` on
Codex), and map named tools to the equivalent host-native capability.

## Workspace CLI

Run commands from the consumer workspace root. Before using the commands below,
select the executable wrapper when available, otherwise the installed CLI:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Resolve this again in each new shell or delegated worker; do not assume shell
variables survive host tool calls. Use `"$PUTNAMI_CLI"` for Putnami commands.

## Arguments

| Flag | Purpose |
|------|---------|
| `<description>` | Free-form description of the feature or change (required) |
| `--scope <target>` | Narrow exploration using an existing native scope/project target or group |
| `--label <label>` | Extra label for a created task; repeatable |
| `--task` | Create a task from the completed plan through the bound tasks provider (requires explicit user authorization); `--issue` is its deprecated spelling |
| `--dry-run` | Print the plan without creating a task |

## Steps

### 1. Understand the request

Parse the feature description to identify:
- **What**: the capability or change being requested
- **Where**: which part of the system it likely targets
- **Why**: the motivation (if stated or inferable)

Build a todo list to track exploration progress.

Reuse an existing implementation plan, task, or study when it is current.
Explore only unanswered decisions and code that may have changed; do not repeat
broad discovery merely because a new implementation turn started.

### 2. Explore the codebase

Your first code-discovery step is a Putnami MCP call, not `Grep`, rg, find, or
`git show`: `putnami.search` to locate code, `putnami.context` to orient on a
project, and `putnami.impact` for what a change breaks. Load them with
ToolSearch `putnami` if they are deferred. Do this even when the request
already names a keyword, a commit, or a file. Use `Grep` only for literal text,
or after a tool answers stale or unavailable.

#### 2a. Load workspace structure

```bash
"$PUTNAMI_CLI" scopes list
"$PUTNAMI_CLI" projects list
```

When the putnami MCP server is available, prefer its `list_projects` tool over shelling out.

#### 2b. Identify primary packages

Based on the feature description, identify which packages are most likely affected:
1. Match keywords against project names and paths
2. For each candidate, use `putnami.context` or the MCP `describe_project` tool (or `"$PUTNAMI_CLI" projects describe <project> --output=jsonl`) to get metadata (type, path, dependencies, exports)
3. Read key source files to understand current implementation

#### 2c. Start from a recipe, then search for existing patterns

Read the recipe index before searching for a similar example:
`.agents/skills/plan/references/recipes.md`, rendered from the framework's
recipe indexes, when present. When a recipe's
intention matches the request, the plan starts from it: cite the recipe's
intention, its sample, and the primitives the tasks use, and do not plan what
its anti-patterns describe. The nearest existing code is often the oldest copy,
not the intended pattern. State "no recipe matches" when none does.

Look for similar features already implemented:
- Start with `putnami.search` for similar code, then the local structural tools (`describe_project`/`deps` for a package's shape and neighbors); use `Grep` only for literal function names, types, or patterns, or after a tool answers stale or unavailable
- Use `Glob` to find files with relevant names
- Read reference implementations to understand conventions

#### 2d. Trace the dependency graph

`putnami.impact`, then the local `impacted`, `why_impacted`, and `deps` (putnami MCP) give dependents and blast radius directly — use them here before reading files. For each affected package (MCP `describe_project`, or the CLI):
```bash
"$PUTNAMI_CLI" projects describe <project>
```

Build a picture of:
- **Upstream**: packages this one imports (may need changes too)
- **Downstream**: packages that import this one (may break or need updates)
- **Cross-cutting**: shared types, config schemas, middleware chains

#### 2e. Read critical files

For each primary package, read:
- Entry points (index.ts, main.go, plugin files)
- Type definitions and interfaces the feature will extend
- Existing test files to understand test patterns
- Package `doc/` folder for documentation patterns

### 3. Collaborate with affected scopes

Resolve the relevant scopes and projects through native configuration, target
selection and actual dependencies. Follow
[scope contributions](../../../.agents/skills/execute/references/scopes.md) for
exact project coverage and contributor attribution. Read existing architecture
contracts when relevant; do not invent a parallel inventory. `--scope` uses
native target semantics: an activated scope selects itself, while a recursive
target includes its descendants. Record the resolved projects explicitly.

Backlog labels and audit groupings are backlog metadata, not another source
of scope ownership.

Before fixing the decomposition, give materially affected scope owners the need,
global direction, current decisions, and versioned contracts. Ask for a distinct
contribution where responsibilities, contracts, or strategic capabilities are
at stake; a routine internal change does not require a voting panel. The
scope owner may propose a substantial internal evolution or reuse of an existing
capability. Record its position, constraints, accepted contribution, scope,
exclusions, acceptance criteria, and reservations using the
[execute records contract](../../../.agents/skills/execute/references/records.md).

An objection explains a concrete constraint and a resolution where possible.
A requested transfer remains unresolved until the receiving owner accepts it;
changing agents cannot erase the objection. Distinguish design agreement from
validation of implementation, and reopen affected agreement when its scope or
contract changes. Escalate decisions outside the mandate with alternatives and
consequences while independent work continues. Ordinary delegated decisions
need no fresh permission. Shared filesystem access makes role attribution a
convention unless the runtime actually enforces separate rights.

Keep scope contributions separate from audit labels. If the requested filter
omits affected scopes or contract owners, include them and explain why.

### 4. Build the implementation plan

#### 4a. Task decomposition

With the relevant scope contributions, define tasks that:
- Have a clear, single responsibility
- Reference specific files to create or modify
- Point to the matching recipe's sample and primitives first, then to other
  existing patterns in the codebase to follow
- Include test and documentation sub-tasks
- Form the smallest useful executable vertical before expanding into later
  layers; for service-impacting work, identify an early running baseline or
  walking skeleton
- Name dependencies, the required local environment, and the existing command
  or harness that will prove real behavior on the implementation branch
- Group into coherent delivery and integration units; a scope does not
  mechanically receive its own task, proposal, or permanent agent
- Keep one intent per proposal. Split by intent, never by size. A plan that
  spans several projects is delivered as an epic: phases on an integration
  branch that matches the workspace `epicBranches`, one commit per phase, each
  phase gated before the next

#### 4b. Task ordering

Order tasks by dependency topology:
1. **Foundation** — shared types, interfaces, config schemas (lower-layer packages like `utils`, `runtime`)
2. **Core implementation** — main feature logic in the primary package
3. **Integration** — connecting to existing systems (registering plugins, adding routes)
4. **Consumer updates** — updating dependent packages if APIs changed
5. **Documentation** — package `doc/` and the documentation roots the repository policy lists under `verification.documentation`

#### 4c. Complexity estimation

Estimate each task's complexity:
- **Small**: isolated and mechanical, following an established pattern
- **Medium**: coordinated changes with a settled design and bounded integration
- **Large**: a new contract, cross-project invariant, or unresolved integration
  shape

Complexity communicates risk and coordination; do not impose arbitrary file,
line, task-count, or time limits on a plan.

### 5. Analyze risks and dependencies

Assess:
- **Breaking changes**: will this change any existing public API?
- **Performance**: does this add hot paths, new allocations, or blocking I/O?
- **Security**: does this handle user input, authentication, or sensitive data?
- **Migration**: does this require data migration, config changes, or deprecation?
- **Test coverage**: what test strategies are needed (unit, integration, edge cases)?

### 6. Return the plan or create the authorized task

For a planning-only request, return the structured plan locally and stop. Do
not create a branch, task, implementation commit, or proposal. When this
plan is the mandatory first phase of an already authorized implementation,
continue through [execute](../execute/SKILL.md) without asking for another
approval. Preserve its scope positions and acceptance requirements in the
same run; planning is not an extra approval ceremony.

Continue with the steps below only when the user explicitly asked to
create/update a task or supplied `--task` with that authorization. The tasks
provider owns labels, identifiers and any backlog mapping; this workflow names
labels only through the repository policy and `--label`.

#### 6a. Build and create the task

**Title**: `[plan] <concise feature summary>`

**Labels**: the policy's `tasks.planLabels`, then every `--label` value.

**Body template**:

```markdown
## Context

<1-3 sentences explaining WHY this feature is needed and what problem it solves>

## Scope

### Affected Packages

| Package | Scope | Role | Complexity |
|---------|--------|------|------------|
| `<package>` | `<scope>` | Primary / Secondary / Dependent | small / medium / large |

### Dependency Impact

<which downstream packages may be affected by the changes>

### Recipe

<the recipe intention this plan starts from, its sample, and the primitives it uses — or "no recipe matches">

## Implementation Plan

### Task 1: <title> (`<package>`)
- **Complexity**: small / medium / large
- **Files**: `<file1>`, `<file2>`
- **Details**: <what to do and how, referencing existing patterns>
- **Pattern reference**: `<path/to/similar/code>` — follow this pattern
- [ ] Implementation
- [ ] Tests
- [ ] Documentation

### Task 2: <title> (`<package>`)
...

### Task Dependencies

<which tasks must complete before others can start>

```text
Task 1 (foundation)
  └── Task 2 (core)
       ├── Task 3 (integration)
       └── Task 4 (consumer update)
            └── Task 5 (documentation)
```

## Risks & Considerations

- **Breaking changes**: <none / list of API changes>
- **Performance**: <impact assessment>
- **Security**: <relevant concerns>
- **Migration**: <needed / not needed>

## Verification Criteria

- [ ] All implementation tasks completed
- [ ] The canonical workspace gate passes with the impacted selection and
      coverage enforcement, using the exact producer record and applicable
      policy/configuration versions (`validate` already includes
      `validate-workspace` when the consumed extension declares that expansion)
- [ ] Each reachable workload or integration vertical has fresh `PASSED` local
      execution evidence from the implementation worktree; any genuinely
      non-runnable change has a justified `NOT APPLICABLE` result
- [ ] Package `doc/` updated for new/changed APIs
- [ ] Documentation roots the repository policy lists updated for user-facing changes
- [ ] No regressions in dependent packages
- [ ] Relevant scope agreements, independent review coverage, and finding
      resolutions are recorded against the implementation revision

`MISSING` and `BLOCKED` proof are non-completion states to report, not ways to
satisfy the verification checklist.

## References

- <links to existing patterns, related tasks, or documentation>

---
_Source: `/plan` | Date: `<YYYY-MM-DD>`_
```

Write the request to a file and create the task. The idempotency key names
this plan, so a repeated or reconciled call returns the same task instead of a
second one:

```bash
jq -n --arg title "[plan] <feature summary>" --rawfile body plan-body.md \
  --argjson labels '<policy tasks.planLabels plus --label values, as a JSON array>' \
  '{title: $title, body: $body, labels: $labels, idempotencyKey: "plan:<short-slug>"}' >plan-task.json
"$PUTNAMI_CLI" tasks create --input-file plan-task.json --output=json
```

Report the created reference and its `url` when the provider supplies one. On
`unresolved`, repeat the identical request once (the key makes it safe) before
reporting; on `unsupported`, the workspace binds no tasks provider: return the
plan locally and say so.

If `--dry-run` was specified, or task creation was not explicitly authorized,
print the title, labels, and body to the terminal instead.

### 7. Summary

```
Plan: <local | created as <task reference>>
  URL: <task url, only when created and the provider supplies one>
  Packages: N affected across M scopes
  Tasks: N total (S small, M medium, L large)
  Scopes: <resolved targets and projects>

Implementation starts only when the user requested it. The implementer reuses
this plan, proves the first executable vertical early, and records both the
ordinary gate and applicable local behavioral proof. The execute loop owns
review, corrections, and delivery within the existing mandate.
```
