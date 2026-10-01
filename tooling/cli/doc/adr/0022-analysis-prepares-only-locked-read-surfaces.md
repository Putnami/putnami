# ADR 0022 — Analysis prepares only locked read surfaces

- **Status**: accepted
- **Scope**: `@putnami/cli`, MCP initialization, extension guidance

## Context

A fresh clone or worktree carries exact extension pins but lacks its
gitignored stable links and machine-local views. Analysis starts before any
build or install: an agent host opens `putnami mcp`, or a user asks for a
workspace description.

Running `putnami install` from that read would run provider
`workspace-install` jobs, dependency installation, context generation, and
lock maintenance. An extension install lifecycle can configure a remote
service; a read must not turn a missing local artifact into Cloud setup or a
change to human guidance. The inverse failure is also unsafe: if exact
preparation fails while a stable link points at another version, discovery
could expose tools and guidance from bytes the lock did not select.

## Decision

### 1. A narrow, bounded preparation path

1. Read the existing lock and select only declared registry extensions with an
   exact lock entry.
2. Materialize each through `EnsureExtensionLocked`: the verified,
   machine-global content-addressed store and the stable-link swap that
   installation uses. No extension code runs.
3. Bound the whole pass to five seconds, including transfer, hashing,
   extraction, and store lock acquisition. On a miss or failure, the read
   continues with core local facts and records the exact extension/version
   failure.
4. CLI and MCP reads use the prepared roots explicitly. A registry extension
   whose preparation failed, or whose pin is absent, is suppressed; it is never
   discovered through `node_modules` or an ambient stable link.

Cold fetches are anonymous: automatic analysis never calls the Cloud
registry-token seam. A private artifact absent from the verified cache is
reported as a miss; an explicit install acquires it.

The path never installs templates, agent content, or dependencies; never runs
`workspace-install`, context generation, Cloud setup, or an extension tool;
never resolves a channel; never writes `putnami.lock.json`; and leaves human
files, credentials, sessions, and `.putnami/agent-artifacts` records untouched.
Graph preparation for a missing workspace index is a separate, lazier step
([ADR 0024](0024-lazy-workspace-initialization.md)).

### 2. MCP initialization instructions

The instructions begin with a self-contained routing rule that fits in the
first 512 bytes, for hosts that read no more. It identifies the server's bound
workspace (an exact path, or a SHA-256 identity for a very long path) and tells
resumed sessions and subagents to compare it with their active worktree; on a
mismatch they use the local CLI from that worktree instead of a parent
session's server. The preamble also tells the agent to pick one advertised
Intelligence tool for structural work, omit workspace arguments, check indexed
freshness and revision provenance, and fall back locally once after stale
evidence or a refusal.

The exact root `AI.md` of each prepared extension follows, with its locked
name and version. A guide must be a regular file of at most 1 MiB. All guides
together are capped at 128 KiB, because they land in the host's system prompt;
a guide that does not fit is named, never truncated, and pointed at its
resource. The same bytes are served as versioned Markdown resources. A missing
exact guide is a versioned JSON `unavailable` resource; core never substitutes
`latest`.

Instructions are session-scoped. No repository path, graph slug, bearer, or
availability observation is persisted in host configuration.

### 3. Language extensions own dependency documentation

The Go and TypeScript extensions resolve framework and library dependencies.
Their read-only documentation tools use the workspace root from the extension
tool request and return their own package, version, and content provenance.
Each selects its toolchain through the resolver its jobs use, with the queried
project supplying the version requirement. Core prepares and advertises those
tools and parses no language lock file.

## Rejected alternatives

- **Run the full install before a read.** Analysis would execute provider
  mutations and dependency installation.
- **Use any resident extension when the lock cannot be restored.** It mixes
  tool schemas and guidance from another version into the workspace.
- **Teach core to parse Go and TypeScript dependency locks.** It duplicates
  language extension authority and diverges on replacements and catalogs.
- **Fetch documentation from `latest`.** It can describe APIs the workspace
  cannot compile against, and is nondeterministic offline.

## Consequences

- A cold read may spend up to five seconds fetching a pinned artifact; warm and
  sibling-worktree reads reuse the store.
- Offline reads without the artifact continue with explicit partial status.
- Explicit install, upgrade, and context generation keep their full lifecycle
  and deterministic output.
