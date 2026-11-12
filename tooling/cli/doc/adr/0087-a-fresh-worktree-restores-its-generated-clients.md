# ADR 0087 — A fresh worktree restores its generated clients

- **Status**: accepted. Amends [ADR 0054](0054-the-contract-edge-fires-on-the-committed-contract.md)
  decision 4 and [ADR 0081](0081-the-core-writes-declared-host-hooks.md).
- **Scope**: `@putnami/cli-model` (`workspace.ProjectMetadataDigest`,
  `GeneratedClientBinding`), `@putnami/cli` (`internal/workspace` bindings, the
  first-run pass, `install` host hooks, version line pathspecs)
- **Date**: 2026-11-11

## Context

Generated clients leave Git
([clientgen ADR 0007](../../../clientgen-extension/doc/adr/0007-a-generated-client-is-built-not-committed.md)).
Three things read the committed manifest today. The workspace binds a client
to its provider from `client.putnami.json`. The client's identity folds the
contract digest that manifest states. A client's version line moves because a
contract change rewrites committed client files.

gopls, `go build` and `tsc` also run before any Putnami command, and find no
client in a fresh worktree.

## Decision

**1. The binding comes from `generatedFrom`.** The workspace reads the member
from the client's `putnami.json` at load and resolves its service id to the
provider whose committed `schema/openapi.json` declares it.

**2. The identity folds the load-time digest.** A client's metadata digest
folds its provider's `ContractSHA256`, computed at load from the committed
contract. This replaces ADR 0054 decision 4: `ContractSHA256` is now identity,
not trace evidence only. An edit to the provider's contract moves every client
key at once. A `generatedFrom` service no provider declares folds a fixed
unresolved marker, and the client's generation fails naming the service.

**3. The contract edge no longer orders jobs.** It keeps change impact and
identity. A client reads the provider's committed contract, so it waits on no
provider job, and a provider may depend on its own client without a cycle.
A provider build still rewrites its contract: it leaves equal bytes in place
and replaces others through a rename. The job context carries the digest the
client's key folds, and `client-generate` fails when the contract it reads has
another digest, so no client is stored under a key its contract does not
match. The next command loads the new contract and generates from it.

**4. A client's version line follows its contract.** A client's line adds its
provider's `ContractPath` to its pathspecs when the contract lies outside the
line. A commit that changes only the contract advances the client; its
conventional type decides the bump.

**5. The first-run pass restores clients.** After the implicit install, the
first-run pass runs `client-generate` for every project that declares it, as an
ordinary cached engine run: a hit restores, a miss renders. It runs once per
fresh worktree, like the install it follows. A project declares
`client-generate` when its `putnami.json` names a provider in `generatedFrom`,
or when it holds a `clientgen.json`: the projects where the command plans a
step. The pass skips `client-generate` itself, which does the same work, and a
hosted run, whose gate plans the clients its builds need and reads a remote
cache this second run would miss.

**6. `install` writes a core SessionStart hook.** When the workspace declares a
generated client, `install`, `init` and `upgrade` write a SessionStart handler
that runs `putnamiw client-generate`. The core owns it: no extension declares
it and no collaboration binding gates it. It keeps auto-install on, so the
first-run pass runs too. It runs `client-generate --all`, so the selection
does not depend on the directory the host starts it in, and sends its output
to stderr, because a host adds a SessionStart hook's stdout to the agent's
context. It is removed when the workspace declares no client. A workspace that
does not load keeps the hook as it is.
Other rules of ADR 0081 hold: only those three commands write the hook files,
and they keep every entry they did not write.

## Consequences

- An agent session in a fresh worktree has its clients before its first edit.
  A human who opens an editor first has them after the first Putnami command.
- A provider change that leaves its contract alone reuses every client entry.
- A contract-only commit publishes a new client version.
