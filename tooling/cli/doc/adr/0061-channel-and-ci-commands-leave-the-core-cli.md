# ADR 0061 — The channel and ci commands leave the core CLI

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`: the command catalog,
  `putnami impact-plan`, `putnami change-plan`, the generated agent guidance)
- **Supersedes in part**: [ADR 0021](0021-publish-version-and-deploy-follow-distribution-v2.md),
  decision 5 and the `putnami ci validate` sentence of decision 7

## Context

The core CLI served two command families that only hosted delivery uses:
`putnami channel set|status`, which moves and reports channels through the
release-set provider, and `putnami ci init|validate|fmt|explain`, which
writes and checks `putnami.ci.json` for a hosted runner. Each one tied the
core to one delivery service, and the extension that owns hosted delivery
already carries both: `@putnami/cloud` serves them as
`putnami cloud channels …` and `putnami cloud ci …`.

## Decision

1. **The two families move to the extension that owns hosted delivery.** The
   core catalog has no `channel` and no `ci` root. The core keeps the
   protocols (`protocols/distribution`, `protocols/ci`) and the generic
   seams they need.
2. **The core refuses a removed root before it does any work.** `putnami
   channel …` and `putnami ci …` are a usage error, raised before the
   workspace bootstrap, the implicit install and planning. The message names
   no extension and no replacement command, because the core does not know
   which extension a workspace installs. An installed extension that declares
   the exact root, as a command group or as a job, serves it as usual.
3. **`impact-plan` is the seam.** An extension that needs the impacted plan of
   a change reads the ImpactPlan of `putnami impact-plan <commands> --base
   <commit>` with the protocol package alone
   ([CI ADR 0005](../../../../protocols/ci/doc/adr/0005-a-change-plan-projects-an-impact-plan.md)).
   A command that no discovered extension declares is a usage error, so a
   misspelled or uninstalled command never yields an empty plan.
4. **`change-plan` leaves once its consumer uses the seam.** Until then it
   stays the projection of `impact-plan` for `lint,test,build,validate`,
   with ChangePlan v1 unchanged.
5. **The generated guidance names no extension.** The gate it states is the
   blocking commands of a usable `putnami.ci.json`, otherwise
   `lint,test,build`. It reads no extension, so it does not change with what
   a machine installed.

## Consequences

- A script that runs `putnami channel …` or `putnami ci …` fails with exit 2
  and has to call the extension's commands instead.
- A workspace whose gate runs more than `lint,test,build`, such as
  `validate`, says so in its `putnami.ci.json`.
- ADR 0021 still describes channel promotion and the release-set provider.
  Only the command that drives them, and the command that validates
  `putnami.ci.json`, moved out of the core.
