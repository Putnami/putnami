# ADR 0009 — A stack profile adapts to each component

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the stack extensions on
  it, first `ruby/extension` (`@putnami/ruby`); builds on
  [ADR 0005](0005-profile-variants-for-a-stack-with-several-builds.md),
  [ADR 0007](0007-nested-markers-excludes-and-runtime-commands.md) and
  [ADR 0013](0013-task-targets-exclusions-and-pub-ranges.md)

## Context

A Ruby or Rails repository does not fit one set of steps run in each
component's directory. One component tests with RSpec, another with
Minitest, a third has no test; a packwerk pack's specs load the
application's `.rspec` and `spec_helper`, and so run from the directory of
the `Gemfile`; one SimpleCov report at that root covers every component a run
loads; `.ruby-version` writes `ruby-3.3.6` and `Gemfile.lock` states the
Bundler version in its `BUNDLED WITH` section; and the stack-root files a
pack's specs load sit beside reports the run itself writes. ADR 0005's
variants name the test runner per component, `tasks.<task>.dir` runs the
steps at the stack root, `toolchain.also` checks Bundler beside Ruby, and
ADR 0013's `!` patterns leave project files out. The rest needs the profile
to say more, or each repository needs a `putnami.json` override per
component.

## Decision

The stack profile gains three members. Each is optional, and a profile that
names none resolves and runs as before, except that a task a profile does
not declare now reads the version files, which moves the key of such a task
once, such as a Java variant's:

- `toolchain.files[].pattern`: a regular expression whose `version` group
  reads the expected version from the file, such as `ruby-3.3.6` in
  `.ruby-version` or the `BUNDLED WITH` section of `Gemfile.lock`. A file
  names a key or a pattern, not both.
- `null` in an override or a variant removes the member, such as a task.
- `!{root}/pattern` in an input set: a stack-root exclusion, such as
  `!{root}/spec/examples.txt` beside `{root}/spec/**`. It reaches the
  manifest's workspace input like the other stack-root patterns.

A covered file a report names by a relative path is found under a source
root the report states, then under the directory the steps ran in, then
under the project directory, so a report a run at the stack root writes is
attributed to the right component. A stack whose every marker directory is a
component may say so with `Stack.IsComponent`, which the attribution asks
instead of the detector.

One rule tightens every profile: a task names at least one input set, as the
design note already states. A task the profile does not declare reads the
version files alone, so an override that adds it keeps the same stack-root
inputs.

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
- **A `tools` member beside the toolchain.** A first version of this
  decision checked Bundler as a named tool. `toolchain.also` checks a further
  command with the selected toolchain, which is what a Bundler beside a
  pinned Ruby needs; two mechanisms for one check would drift.

## Consequences

- A stack extension states per-component differences in its profile and its
  variants, and the conformance kit holds its manifest to the profile's
  stack-root files.
- An override cannot add a stack-root input the shipped profile does not
  declare; dropping a stack-root exclusion changes nothing, since the
  manifest's workspace input keeps it.
