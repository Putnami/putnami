# Qualify a workload

You will prove that a workload you changed boots through its production entrypoint, reaches readiness with the workloads it runs with, and answers real requests — with one command and one verdict, and no hand-written smoke test. The same verdict works on your machine before a pull request and against a deployment after it ships.

`putnami lint,test,build,validate` proves a change statically. `putnami qualify` proves it by running it. Keep both: the gate is static evidence, the verdict is execution evidence.

## Steps

### 1) Declare what the workload runs with

A workload that calls another workload names it in `runsWith`, in its `putnami.json` (or under `putnami.runsWith` in a TypeScript `package.json`):

```json
{
  "name": "@example/go-items-consumer",
  "runsWith": ["/services/items-api"]
}
```

Each entry resolves by project name, then by project id. That list, followed transitively, is the whole composition. Putnami never infers a runtime dependency from the build graph: compiling against a generated client does not mean you run the provider.

### 2) Develop against the composition with `compose`

For the daily loop, serve the workload together with everything it runs with:

```bash
putnami compose @example/go-items-consumer
```

Every member binds an ephemeral port behind a local proxy whose URL stays stable across restarts. The provider's URL reaches the consumer through its `clients` configuration, and each datasource declared in `infra/requirements.json` gets its own database for the length of the run. Dependencies run production-mode; the target watches your edits. `Ctrl-C` stops every process and drops every database.

### 3) Qualify on your machine before a pull request

When the change is ready, run the local proof:

```bash
putnami qualify @example/go-items-consumer --target local
```

One invocation composes the workload production-mode, waits for each member's typed ready event, waits for the workload's application to report that its whole startup completed, sends the smoke contract, and tears everything down, whatever happened. A workload whose route inventory declares `GET /readyz`, or that you qualify with `--platform-prefix`, is polled there instead; `/readyz` answers `200` only once the same startup completed. The contract is derived from the workload's route inventory: every exact, public `GET` or `HEAD` route except the platform endpoints, each expected to answer below `500`. Print it without running anything:

```bash
putnami qualify @example/go-items-consumer --print-contract
```

A TypeScript workload without a committed route inventory needs a build first, which produces one:

```bash
putnami build --projects @example/web
putnami qualify @example/web --target local
```

Without it the verdict is `unsupported`, never `passed`.

### 4) Read the verdict

Human output prints one line per phase and a final line naming the tree it proved. With `--output=json`, the verdict is the result's `data`. Three members answer everything:

| Member | What to check |
|---|---|
| `state` | `passed` is the only pass and the only state that exits `0`. `failed`, `unsupported`, `timed_out`, `composition_failed`, `digest_mismatch` and the others exit `1`, and the failing phase carries the diagnostic. |
| `binding` | `{kind: "tree", fingerprint, dirty, headSHA}` names the exact worktree, uncommitted changes included. It is the same digest a gate session records as `tree.fingerprint`, so a verdict and a gate prove the same tree when the two are equal. A file that changes during the run makes the verdict `digest_mismatch`. |
| `cleanup` | `state: "clean"` means every process, proxy and database was released; `partial` lists `leftovers`, and the verdict is never `passed`. |

### 5) Qualify a deployment after it ships

The runner is the same against any URL. A URL target must name the build it expects, which the workload reports on `/version`:

```bash
putnami qualify @example/go-items-consumer --target https://staging.example.com --expect-sha "$(git rev-parse HEAD)" --output=json
```

A different or missing sha is `digest_mismatch`: a green verdict on a stale deployment proves nothing. A CI system qualifies a pull request preview the same way, with the preview URL and the pull request head sha.

## When qualification does not apply

Qualify every workload your change reaches: a workload you changed, or one whose `runsWith` closure includes a project you changed. A library-only or docs-only change that reaches no workload has no execution proof to run. Report it as `NOT APPLICABLE`, with the reason, instead of claiming a pass.

You now have one command that proves a changed workload runs with its real composition, a verdict that names the exact tree or build it proved, and the same proof for your machine, a pull request preview, and a deployed environment.
