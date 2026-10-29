# Prove

Prove shows the change works when it runs: tests show the specs hold, and
qualification runs each workload the change reaches.

A changed project reaches a workload when it is one, or when a workload's
`runsWith` closure includes it. For each, on the tree of the final gate, run
`"$PUTNAMI_CLI" qualify <project> --target local --output=json` and save its
output unchanged. A TypeScript workload without a committed route inventory
needs `"$PUTNAMI_CLI" build --projects <id>` first, otherwise the verdict is
`unsupported`. The proof holds only with `state` `passed`, `cleanup.state`
`clean`, and a `binding.fingerprint` equal to the gate session's
`tree.fingerprint`.

- A screen change: capture the screens the change touches into
  `.putnami/<issue>/`, for the owner to validate.
- A generated client: exercise the provider declaration, the generation, the
  emitted client and a real consumer call.
- An unsupported qualifier is a missing adapter, not success: use an existing
  execution harness the repository allows, and record that evidence apart.
- A change that reaches no workload is not applicable, with the reason from
  the project graph.

Keep internal providers and consumers real; use doubles only at external
boundaries, and say so. The gate, the qualification and the functional
acceptance are separate evidence: a response below 500 proves neither
authorization nor business behavior. Missing, skipped or stale proof is never
reported as passed.
