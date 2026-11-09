# ADR 0007 — A stack profile adapts to each component

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the stack extensions on
  it, first `ruby/extension` (`@putnami/ruby`); builds on
  [ADR 0005](0005-profile-variants-for-a-stack-with-several-builds.md)

## Context

A Ruby or Rails repository does not fit one set of steps run in each
component's directory. One component tests with RSpec, another with
Minitest, a third has no test; a packwerk pack's specs load the
application's `.rspec` and `spec_helper`, and so run from the directory of
the `Gemfile`; one SimpleCov report at that root covers every component a run
loads; Bundler is a second tool whose version the lockfile states; and the
files RuboCop inspects sit beside a `vendor/` the inputs must leave out.
ADR 0005's variants name the test runner per component. The rest needs the
profile to say more, or each repository needs a `putnami.json` override per
component.

## Decision

The stack profile gains five members. Each is optional, and a profile that
names none, such as the Rust, PHP or Java one, resolves and runs as before:

- `toolchain.files[].pattern`: a regular expression whose `version` group
  reads the expected version from the file, such as `ruby-3.3.6` in
  `.ruby-version` or the `BUNDLED WITH` section of `Gemfile.lock`.
- `tools`: further tools, by name, whose versions are checked before the
  steps like the toolchain and with its rules, such as Bundler. Unlike the
  tools `Stack.Tools` installs (ADR 0004), nothing installs them. Their files
  are inputs of every task, and the probe records their expected versions.
- `null` in an override or a variant removes the member, such as a task.
- `tasks.<task>.cwd`: `{project}`, the default, or `{root}`, the stack root
  the steps run in.
- `!` patterns in an input set: a pattern that leaves the files it matches
  out of the task's inputs, such as `!vendor/**` beside `**/*.rb`, or
  `!{root}/spec/examples.txt` beside `{root}/spec/**` at the stack root, in
  the protocol's own pattern language. A stack-root exclusion reaches the
  manifest's workspace input like the other stack-root patterns.

A covered file a report names by a relative path is found under a source
root the report states and under the directory the steps ran in, before the
project directory, so a report a run at the stack root writes is attributed
to the right component. A marker such as `*.gemspec` is matched as a
pattern, and a stack whose every marker directory is a component may say so
with `Stack.IsComponent`, which the attribution asks instead of the detector.

Two rules tighten every profile: a task names at least one input set, as the
design note already states, and a tool of `tools` names no `provisioned`,
since nothing provides it.

## Rejected alternatives

- **Variants the core picks from the project's files.** A first version of
  this decision had each variant name the files that select it, read at
  every task. ADR 0005 already lets the detector name the variant; two
  mechanisms for one choice would let them disagree. The Ruby detector names
  it from the spec and test files, which its workspace adapter hashes.
- **Steps in the component directory with the root's options passed as
  flags.** `rspec --options` replaces the component's own `.rspec`, and
  SimpleCov's root, which a `spec_helper` often sets, would still decide where
  the report lands.

## Consequences

- A stack extension states per-component differences in its profile and its
  variants, and the conformance kit holds its manifest to the profile's
  stack-root files and tools.
- An override cannot add a stack-root input the shipped profile does not
  declare; dropping a stack-root exclusion changes nothing, since the
  manifest's workspace input keeps it.
