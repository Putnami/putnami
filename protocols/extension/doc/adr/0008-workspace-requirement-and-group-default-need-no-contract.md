# ADR 0008 — A subcommand's workspace requirement and a group default need no contract increment

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `commandGroups` member of `putnami.extension.json`, and
  `go.putnami.dev/protocol/cli` (`protocols/cli/contract.go`)

## Context

Two manifest members let an extension offer a command to a person who has no
workspace:

- `commandGroups.<group>.default` names the subcommand `putnami <group>` runs
  when no subcommand word follows.
- `commandGroups.<group>.subcommands.<sub>.workspace` is `required` (the
  meaning of an absent value) or `optional`. An `optional` subcommand is
  interactive and runs outside any workspace from an extension pinned in the
  user scope ([CLI ADR 0051](../../../../tooling/cli/doc/adr/0051-user-scope-extensions-run-without-a-workspace.md)).

[ADR 0006](0006-agent-content-is-an-additive-contract.md) gives a member that
an older reader must not silently drop a new additive contract.

## Decision

Neither member moves the contract. `RequiredCLIContract` stays 4 for a
manifest that declares them, and `LatestContract` does not change.

The test is what an older reader does with the silent drop. For these members
the drop is the behavior the manifest had before it declared them:

- A CLI that drops `default` prints the group help for `putnami <group>` and
  exits 0. It runs nothing the author did not name.
- A CLI that drops `workspace` treats every subcommand as `required`. It has
  no user scope, so it never runs a subcommand outside a workspace, and
  inside one the member has no meaning.

The strict parser and the schema carry the rules: `workspace` is `required`
or `optional` (`invalid-enum`); `optional` requires `interactive: true`
(`invalid-workspace-requirement`, and a schema `if`/`then`); `default` names
one of the group's own subcommands (`unresolved-default-subcommand`, strict
parser only, because the schema cannot compare a value with a sibling's
keys). `SubcommandDefinition.RunsWithoutWorkspace` is true only for
`optional` with `interactive`, so a manifest the permissive loader read
without validation cannot widen where a subcommand runs.

## Rejected alternatives

- **A contract 6 for both members.** A contract-5 CLI would refuse every
  package that declares them, inside workspaces where neither member has an
  effect.
- **Allow `optional` on a non-interactive subcommand.** A non-interactive
  subcommand runs through the scheduler, which plans over workspace projects
  and writes session records under the workspace; outside one there is
  neither.

## Consequences

- A manifest that declares either member keeps its `cliContract` stamp and
  loads in every CLI that reads that stamp.
- The next member an older reader must not drop still follows ADR 0006.
