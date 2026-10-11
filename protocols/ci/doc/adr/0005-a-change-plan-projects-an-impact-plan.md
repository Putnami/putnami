# ADR 0005 — a change plan projects an impact plan

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`), `@putnami/cli`
  (`putnami impact-plan`, `putnami change-plan`)

## Context

An extension that plans work over a change needs what the engine already
computes for `putnami change-plan`: the changed files, the impacted projects,
and the tasks the engine plans for them. `change-plan` plans one fixed command
list, `lint,test,build,validate`, and needs an `origin` remote, because its
ChangePlan is an admission document for one repository. Its ChangePlan v1
bytes and digest are frozen: a runner admits plans by that digest. The only
other way to reach the engine's plan was to import the CLI's internal
packages, which an extension cannot do.

## Decision

1. **The ImpactPlan is the document an extension reads.** `putnami
   impact-plan <commands> --base <commit>` emits it for the command list the
   caller names. It holds the members of a ChangePlan that describe the change,
   with the same types, plus the ordered command list.
2. **It names no repository and has no digest.** Repository identity is
   the consumer's input, so `impact-plan` needs no remote. The document a
   consumer derives holds the identity and the digest that bind it.
3. **A ChangePlan is a projection of an ImpactPlan.**
   `ChangePlanFromImpactPlan(plan, repository)` copies the shared members,
   derives the transitive dependents, adds the repository and stamps the
   digest. The CLI builds every ChangePlan through it, so the two documents
   cannot disagree about one range.
4. **ChangePlan v1 is unchanged.** `change-plan` keeps its command list, its
   checks, its errors and its bytes. The shared list checks moved into one
   function that both validators call with the same messages.
5. **The ImpactPlan has a version and a corpus, and no schema**, as the
   ChangePlan does. `fixtures/impact-plan` pairs each valid plan with the
   ChangePlan fixture its projection must equal.

## Rejected alternatives

- **A command list member on the ChangePlan.** It changes the canonical bytes
  and the digest of every v1 plan, so it needs a v2 that every runner must
  learn first.
- **A `--commands` flag on `change-plan`.** A ChangePlan does not say which
  commands it planned, so two plans of one range for two lists would be
  indistinguishable to the runner that admits them.
- **An extension SDK call into the engine.** It ties an extension to the
  engine's Go types and version, which the structured output of a command
  does not.

## Consequences

- A change to a shared member changes both documents, and is a new ChangePlan
  version as before.
- An extension pins a CLI release that has `impact-plan`. An older CLI does
  not refuse the command as unknown: it reads `impact-plan` as a job name,
  may run the implicit workspace install, warns that `--base` is not
  declared, and fails with a project selection error (exit 2). An extension
  reads the CLI version from `putnami --version --output=json` before it
  calls `impact-plan`.
- `impact-plan` refuses, as a usage error, a command that no discovered
  extension declares, so a misspelled or uninstalled command never yields an
  empty plan.
- `impact-plan` refuses an alias that expands to several commands: the plan
  names each command once, by its own name.
