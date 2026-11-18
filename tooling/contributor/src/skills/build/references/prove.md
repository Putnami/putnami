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

- A web change, one that touches a page of a web project (its committed
  `schema/http-routes.json` declares `file-route` GET routes): a page whose
  `proves` names a requirement the change adds or modifies, or whose own page
  file changed. The screens section below.
- A generated client: exercise the provider declaration, the generation, the
  emitted client and a real consumer call.
- An unsupported qualifier is a missing adapter, not success: use an existing
  execution harness the repository allows, and record that evidence apart.
- A change that reaches no workload is not applicable, with the reason from
  the project graph.

## Screens

Screenshots are the proof of a web change. On the committed tree of the
final gate, for each web project with a touched page:

1. Serve the head with `"$PUTNAMI_CLI" serve <project> --port <head port>`,
   and the base from a worktree of `git merge-base origin/<base> HEAD` on
   another port. A change that only adds pages needs no base server.
2. Run `"$PUTNAMI_CLI" change screenshots --project <dir> --base <base>
   --base-url <base URL> --head-url <head URL> --out
   .putnami/<issue>/screenshots/<project>`. Pass `--storage-state` with a
   Playwright `storageState` file when the pages need a sign-in, and
   `--fixtures` with the values of each touched template route that declares
   no `proves`: a page that proves a requirement is visited at its `at`. A
   shared component the change edits touches no page: name with `--route` a
   page you want to see it on. A template
   it refuses, an error status or a failed navigation is a proof to repair,
   not a page to skip.
3. Read every capture it lists, before and after, desktop and mobile, light
   and dark, against the design rules the workspace states (its
   `.agents/constraints.md`, the project's docs and design system) and these
   checks: text stays legible against its background in both schemes;
   nothing is clipped, overlapping or overflowing at mobile width; controls
   and focus states are visible; the after side keeps what the before side
   got right. Fix what you find, commit, and capture again.
4. Keep the manifest, `screenshots.json`, for the reviewer and for
   `change finalize --screenshots`. A web change you cannot capture, such as
   one whose pages need a backend the session cannot reach, goes to the owner
   with the reason, which becomes `--waive-screenshots <reason>` only on their
   word.

Keep internal providers and consumers real; use doubles only at external
boundaries, and say so. The gate, the qualification and the functional
acceptance are separate evidence: a response below 500 proves neither
authorization nor business behavior. Missing, skipped or stale proof is never
reported as passed.
