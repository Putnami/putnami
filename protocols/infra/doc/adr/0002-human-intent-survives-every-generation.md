# ADR 0002: Human intent lives in files generators never rewrite

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/infra` (`protocols/infra`)

## Context

Most infrastructure facts are derivable, and generators emit them into
`infra/requirements.json`. Two facts belong to a human: workload runtime intent
(ingress, scaling, domain, protocol toggles), and the decision to drop a
requirement a generator declared. Merge precedence can replace a value but never
remove an entry, and anything written into the generated file is overwritten by
the next generation.

## Decision

Human intent lives in files a generator never writes. The aggregator applies it
in this order:

1. `<workload>/infra/runtime.json`: workload runtime intent. When it is absent,
   the aggregator synthesizes `infra.DefaultRuntime()` and writes a visible
   sidecar at `<workload>/.gen/infra/runtime.json`. When the authored file
   exists it wins and the sidecar is removed, so two competing values never sit
   on disk.
2. `<workload>/infra/overrides.json`: suppression. `ApplyOverrides` runs on the
   merged manifest and drops entries matching an `ignore` rule by merge
   identity. A rule that matches nothing warns (`infra.unused_override`).
3. Contributor precedence settles scalar disagreements only. A
   developer-authored `manual` declaration beats a `framework:*` one; a
   same-rank disagreement is an error (`infra.conflicting_value`).

Both human files are workload-root concerns; a library's per-project manifest
rejects them (`infra.runtime_in_library`).

## Rejected alternatives

- **Preserve human edits inside the generated file.** The generator would parse
  and re-emit a human's file on every build, one bug away from deleting intent.
- **Express removal as precedence.** A tombstone value (`null`,
  `disabled: true`) would burden every consumer.
- **Edit the aggregated `.gen/requirements.json`.** It is a build artifact,
  regenerated and absent from a clean checkout.
- **Drop an unmatched override silently.** A typo would leave the requirement
  provisioned.
- **Make the runtime block suppressible.** It always exists; changing it is
  editing `runtime.json`.

## Consequences

- A workload has three files: generated requirements, authored runtime intent,
  authored overrides. The README's merge order is the contract.
- The sidecar is regenerated on every build, so a `DefaultRuntime()` change
  propagates without developer action; nobody edits the sidecar.
- An override against a renamed resource warns instead of silently doing
  nothing.
- Deploy activation reads the committed files in `infra/`, never
  `.gen/requirements.json`.
