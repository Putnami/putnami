# ADR 0021 — A tool that runs only its stated version is not probed

- **Status**: accepted
- **Date**: 2026-11-25
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the Pants integration
  (`integrations/pants`)
- **Amends**: [ADR 0010](0010-a-stack-shapes-its-components-tasks-and-versions.md),
  whose check compares what the toolchain command prints

## Context

Every task, and every delegated verb ([ADR 0019](0019-a-delegated-verb-runs-as-one-tool-command.md)),
runs its toolchain check first: it starts `toolchain.command` and compares the
version it prints with the version the toolchain files state. For Pants that
command is `pants --version`, which goes through pantsd. Measured on a Pants
2.31.0 repository of 16 Go packages, warm, the probe costs 0.4 to 0.6 s, and
the `pants package` that follows replays 37 workunits, 9 of them processes,
instead of 5: a warm delegated build takes 1.37 to 1.54 s, past the bound of a
delegated verb, at most twice the tool alone plus one second, here 1.26 s.
With `--version` answered without pantsd it takes 0.42 to 0.54 s.
`pants --no-pantsd --version` costs 3 s.

The Pants launcher (scie-pants) runs exactly the release `[GLOBAL]
pants_version` of `pants.toml` names, or the one the `PANTS_VERSION` variable
names when it is set, and Pants itself refuses to start when that option is
not its own version. The version the probe reads is therefore the one the
files and that variable state, whenever they state an exact one.

## Decision

A toolchain check may declare `stated`: the tool runs only the version its
files state, or the one the variable `stated.env` names when that variable is
set in the task's environment. When that version is exact, the check of the
toolchain on `PATH` takes it as the machine's version and starts no command:
it only looks the command up on `PATH`, so a missing tool is still
`stacks.toolchain_missing`, and it compares as before, so a variable that
names another release than the files is still `stacks.toolchain_mismatch`.

The files and the variable are read on every run, and the command is looked up
on every run: a change to the pinned version, to the variable or to the tool on
`PATH` is seen by the next run, with nothing recorded between runs.

Without an exact version (no file states one and the variable is unset, or a
range), the check runs the command as before. A provisioned toolchain and the
further checks (`toolchain.also`) still run their command: nothing says they
run only the stated version.

The Pants profile declares `"stated": {"env": "PANTS_VERSION"}`. No other
profile does: each of their tools runs whatever release is installed.

## Rejected alternatives

- **Record the probe's answer, keyed by the launcher's file identity and the
  content of the version files and variables.** It saves the same probe, but
  adds a record to keep, invalidate and protect against concurrent writers,
  for a version Pants already enforces at start.
- **`pants --no-pantsd --version`.** It leaves pantsd's state alone, but costs
  3 s on every verb.
- **Skip the check for a delegated stack.** The key would lose nothing, but a
  repository with no `pants` on `PATH` would fail in the tool's step instead
  of with `stacks.toolchain_missing`, and a `PANTS_VERSION` that contradicts
  `pants.toml` would no longer be reported.

## Consequences

- A warm delegated Pants verb starts one Pants command, its own.
- A Pants started outside the launcher, which would run whatever release is
  installed, is not compared with the stated version by Putnami: Pants's own
  check of `pants_version` reports it when the command runs.
- The check does not read the launcher inputs that make it run something
  other than the stated release: `PANTS_SOURCE`, `PANTS_TOML`, or a
  `.pants.bootstrap` that sets `PANTS_VERSION`. The launcher then sets
  `PANTS_VERSION` to what it runs, so Pants's own check passes too, and the
  task runs that Pants without a mismatch. These are settings a developer
  chooses; Pants tasks keep their cache off, so no result is recorded under
  the stated version. Configuration overlays (`.pants.rc`,
  `PANTS_CONFIG_FILES`) do not change the release the launcher runs, and a
  `pants` started outside the launcher refuses a version other than the
  stated one.
- A profile whose tool enforces its stated version the same way may declare
  `stated`; one that does not must keep probing. A further check
  (`toolchain.also`) cannot declare it, and a project's override may add or
  remove it like any other toolchain member.
