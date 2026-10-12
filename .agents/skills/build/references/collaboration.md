# Tasks, proposals and memory

Every workflow reaches work items, change proposals and shared memory through
the three collaboration contracts, never through a backend's own client. The
workspace binds each contract to a provider extension in the
`options.collaboration` block of `putnami.workspace.json`; the provider owns
the backend, its credentials, its identifiers and its label or state mapping.
Local Git operations (commit, branch, push, rebase) stay with Git.

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
"$PUTNAMI_CLI" tasks capabilities --output=json
"$PUTNAMI_CLI" tasks get --input '{"ref":{"source":"<source>","id":"<id>"}}' --output=json
"$PUTNAMI_CLI" proposals find --input '{"change":{"base":"<base>","head":"<branch>"}}' --output=json
"$PUTNAMI_CLI" memory mission --input '{"mission":"<mission>"}' --output=json
```

The MCP server offers the same operations as `<contract>.<operation>` tools
(`tasks.get`, `proposals.upsert`, `memory.checkpoint`, ...) with the same
request and envelope. Use whichever entry path the host exposes.

## Read the envelope

Every call prints one envelope: `outcome`, then `result` or `error`
(`message`, `reason`, `retryable`, `current`, `reconcile`). The command exits 0
only for `ok`.

| Outcome | What the workflow does |
|---|---|
| `ok` | Continue with `result`. |
| `not_found` | The reference names nothing: report it; never create a substitute silently. |
| `conflict` | Nothing was written. Re-read the item, decide again from its current state, then repeat once. |
| `unsupported` | The contract is unbound or the provider lacks the operation. Report the missing capability; take the documented fallback or stop. Never report the step as done. |
| `invalid` | Fix the request; nothing ran. |
| `denied` | Stop and report: credentials and permissions belong to the workspace. |
| `unavailable` | No write happened. Repeat only when `retryable` is true. |
| `unresolved` | A write may have happened. Stop automatic retries, run the read `reconcile` names, and repeat only a write the read proves absent (an idempotent repeat with the same key or identity is safe). If the read cannot settle it, report the operation as unresolved. |

Store a reference as the whole `{source, id}` pair and pass it back unchanged;
never parse it or rebuild it from a display URL. A `url` is for people.
Revisions are opaque tokens: send `expectedRevision` only when the capability
document shows the operation's `preconditions` as `atomic` or `checked`, and
treat `checked` as best effort against a concurrent writer.

## Capabilities differ; say so

Run `<contract> capabilities` before relying on an optional operation or on
hosted checks. A provider may report checks `unsupported`, offer no `merge`,
`assign` or `claim`, or state `preconditions: none`. Report the subset used.
An assignee is information, never an exclusive lease; only `claim` on a
provider that offers it is exclusive.

## Memory is context, not evidence

When `memory capabilities` reports status `bound`:

- At start, read `memory mission` for the run's mission id and `memory context`
  for the affected projects. A record carries identity, provenance, revision
  and freshness; weigh stale records accordingly. Memory never overrides the
  current mandate, repository decisions or producer records.
- At a checkpoint, save a compact state with `memory checkpoint`: `mission`,
  `precondition` (`mustNotExist: true` for the first write, then the
  `expectedRevision` the last read or write returned), an `idempotencyKey`
  that names this logical write (for example `<mission>:<step>:<n>`), `title`,
  `content` and `evidence` entries that point at the actual gate, review and
  qualification records. The content names the models that worked on the
  mission, with their versions, because no commit or proposal does. A
  checkpoint references evidence; it never states a verdict of its own.
- On `conflict`, read the mission again, merge what changed, and write against
  the returned revision. On `unresolved`, read the mission and repeat the same
  request, which replays when the first write landed.

When memory is unbound, keep the checkpoint in the ignored run directory
(`.context/execute/<run>/`) and state that it does not survive the workspace.
Never name a storage location, repository or file layout for memory: the
provider owns them.
