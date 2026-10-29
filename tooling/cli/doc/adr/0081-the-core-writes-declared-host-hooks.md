# ADR 0081 — The core writes the host hooks an extension declares

- **Status**: accepted
- **Amended**: 2026-10-28, by
  [ADR 0082](0082-putnami-serves-no-mcp-server.md): hooks no longer share a
  launcher with an MCP server, which the core no longer serves
- **Amended**: 2026-10-29, by
  [ADR 0004 of the collaboration contracts](../../../../protocols/collaboration/doc/adr/0004-a-provider-is-declared-under-providers-and-speaks-its-own-wire.md):
  `hostHooks[].mcpTools` is deleted (decision 11)
- **Date**: 2026-10-26
- **Amends**: [ADR 0040](0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md)
  and [ADR 0047](0047-extension-owned-agent-content.md)
- **Scope**: `@putnami/cli` (`internal/commands/agentctx`,
  `internal/commands/lifecycle`, `internal/extension`), the extension manifest
  (`protocols/extension`, `hostHooks`), the CLI contract (`protocols/cli`,
  rung 17), feature `cli/agent-workflow-lifecycle`

## Context

An agent host runs a shell command on an event of its own: a session starts,
a tool returns, a turn stops. Claude Code reads these hooks from
`.claude/settings.json`, and Codex reads them from `.codex/hooks.json`. An
extension that reports agent sessions needs its command to run on those
events.

Until now a person wrote these hooks by hand in each workspace, and kept them
in step with the extension's command. ADR 0040 made the host files host
policy that Putnami does not write. ADR 0047 made an extension the only source
of the agent content a workspace opts into.

Two facts make the hooks risky to write:

- The files are shared. The person keeps permissions, environment and their
  own hooks there, and the file has no room for a record of what Putnami
  wrote.
- A host reads the hook's exit code. Claude Code treats exit code 2 from a
  Stop hook as "continue the turn", and from an `asyncRewake` hook as "start
  a turn". The CLI itself exits 2 on a usage or bootstrap error.

## Decision

1. **An extension declares its hooks in the manifest.** The top-level
   `hostHooks` member names the collaboration contract the hooks need, the
   command path they run, and the entries for each host. The path starts with
   a command group of the extension and a top-level subcommand that sets
   `interactive: true`. The hosts are `claude-code` and `codex`, each with a
   closed list of events. Only Claude Code takes a `wake` entry. The member
   needs `agentContent` and raises the required contract to 17
   (`HostHooksContract`). The validation rules are in
   `protocols/extension/doc/04-validation.md`.
2. **Only an explicit command writes them.** `putnami install`, `putnami init`
   and `putnami upgrade` write the hooks, next to the MCP server registration.
   `putnami upgrade --dry-run` lists the files it would change. The implicit
   first-use install, the first-run pass, `putnami context generate` and the
   MCP server start never write them, so no other command changes a committed
   file.
3. **Three conditions turn an extension's hooks on.** The workspace opts into
   the extension's agent content (`extension:<name>` in `agentArtifacts`),
   binds the contract the hooks need under `options.collaboration`, and has
   the extension installed. A contract this CLI does not know counts as not
   bound.
4. **A handler is Putnami's by its command, and no record says so.** A
   handler is Putnami's when its command runs a known launcher followed by the
   command path an installed extension declares, as whole shell words. The
   launcher is a command word: it starts the command, or follows a shell
   operator (`;`, `&`, `|`, `(` or a new line), a reserved word such as `then`,
   or `NAME=value` words. `echo putnami agent hook` prints the text and is the
   person's. The known launchers are `putnami`, `./putnamiw`,
   `"$CLAUDE_PROJECT_DIR"/putnamiw` and
   `"$(git rev-parse --show-toplevel)"/putnamiw`. A hook that a person wrote
   for the same command is therefore Putnami's, and the next run replaces it.
   Every other handler is the person's.
5. **The merge keeps the person's text.** A run replaces Putnami's handlers in
   place and never adds a second one. It appends a missing hook at the end of
   its event. Every other key, event, group and handler keeps its bytes and
   its place; a structure the run edits takes the file's own indent. A group
   that mixes the person's handlers with Putnami's is split, and the person's
   handlers stay in place. A run plans every file before it writes, writes
   each changed file once, and changes nothing when the hooks are current.
   `.codex/hooks.json` is created when it is absent.
6. **A run removes the hooks when a condition goes away.** Without the opt-in
   or the binding, a run removes Putnami's handlers, then an event, a `hooks`
   object or a file that they alone filled. An extension removed from the
   workspace declares no command path any more, so its handlers are no longer
   recognized as Putnami's, and they stay.
7. **A file that cannot be edited safely is left untouched.** A file that
   cannot be read, is not valid JSON, is not an object, repeats `hooks` or an
   event, has a `hooks` or event member of the wrong type, or is a link keeps
   its bytes. The run warns and names the file, and the command carries on. An
   opted-in extension whose declaration this CLI cannot read keeps its
   handlers, with a warning.
8. **No exit code of the CLI reaches the host as is.** An ordinary hook ends
   with `|| true`, so it always exits 0. A wake hook ends with
   `; s=$?; [ "$s" -eq 75 ] && exit 2; exit 0`: it asks for a turn by exiting
   75 (`EX_TEMPFAIL`), which no CLI or wrapper failure produces. The CLI exits
   0, 1, 2, 3, 4 or 130, and the wrapper exits 0 or 1, so a failure never
   starts a turn.
9. **The launcher passes the command's exit code through.** `putnamiw` ends
   with `exec`, and a version relaunch replaces the process on Unix
   (`syscall.Exec`) or exits with the child's code on Windows. An interactive
   extension subcommand exits with its child's positive exit code unchanged
   (`interactiveExitCode` in `internal/cli/extension_command.go`); a
   non-interactive one maps every failure to 1. This is why the command path
   must start with an interactive subcommand.
10. **A hook starts with the workspace's launcher, and never installs.** It
    uses the launcher the MCP server registration chooses. In a source
    workspace, which runs `./putnamiw`, a hook names the wrapper from the root
    the host provides: `"$CLAUDE_PROJECT_DIR"/putnamiw` in Claude Code and
    `"$(git rev-parse --show-toplevel)"/putnamiw` in Codex. Every hook command
    starts with `PUTNAMI_NO_AUTO_INSTALL=1`, the switch that turns the
    first-use install off. That install can outlast the host's timeout, and it
    prints on standard output, which the host reads as the hook's answer. A
    workspace that is not installed yet gets no hook output until a person
    runs a command.
11. **Matchers follow each host.** Claude Code reads `A|B`. Codex reads a
    regular expression that matches a substring, so its matcher is `^(A|B)$`.
    A Putnami MCP tool is named `mcp__putnami__<tool>`, with each dot of the
    tool name turned into an underscore. Putnami writes the `putnami` server
    into `.mcp.json` for Claude Code. It does not write the Codex
    configuration, so the hooks assume Codex registers the server under the
    same name.
12. **A Codex change reports the trust step.** Codex runs a new or changed
    hook only after the person reviews and trusts it with `/hooks`. A run that
    creates or changes `.codex/hooks.json` says so.

## Amends

- **ADR 0040.** `.claude/settings.json` is no longer only hand-authored: the
  explicit lifecycle commands manage the `hooks` handlers an opted-in
  extension declares, by decisions 4 to 7. The session-start pass still
  writes no host file.
- **ADR 0047.** An extension's agent content opt-in also turns on the host
  hooks it declares, when the contract they need is bound. The hooks are read
  from the same verified manifest as the content: an installed extension
  through its lock pin and manifest digest, a local one from its directory.

## Consequences

- A workspace that opts into an extension's content and binds the contract
  gets its hooks from `putnami install`, and loses them when it drops either.
- A person may keep their own hooks in the same files. A hook they write for
  the extension's command becomes Putnami's.
- Removing an extension from `extensions` leaves its handlers in the files;
  the person removes them, or drops the opt-in first and runs
  `putnami install`.
- An extension that needs another host, event or field needs a new contract
  rung: the lists are closed, and the strict parse refuses an unknown member.
- A manifest stamped below 17 that declares `hostHooks` is refused with
  `putnami extensions update` as the remedy.
- A Putnami MCP tool named with a hyphen is refused in `mcpTools`: no host
  rendering of the hyphen is confirmed.
