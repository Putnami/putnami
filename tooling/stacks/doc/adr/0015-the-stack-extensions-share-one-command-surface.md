# ADR 0015 — The stack extensions share one command surface

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the seven stack
  extensions: `@putnami/rust`, `java`, `swift`, `dart`, `dotnet`, `ruby` and
  `php`

## Context

The seven stack extensions grew in parallel, each from the shape the core
gave the first one, and their commands drifted where the stacks do not
differ:

- A test task with no coverage report failed in Swift, Dart, .NET and Ruby
  with `stacks.report_missing`, ran without coverage with a warning in Rust and
  PHP (`withoutCoverage`), and silently read no coverage in Java (an optional
  report). In .NET and Ruby the report depends on the project's own setup, a
  Coverlet collector or SimpleCov, not on the toolchain.
- The image of an application listened on 8080 in six stacks with no flag, and
  on 3000 with a `--port` flag in Rust.
- `serve` took `--args` in Rust only; `run` and `serve` took `--entrypoint`
  where a package holds several programs, except in Rust.
- Rust read only the kebab-case form of the package options, where the others
  also read the camelCase one (`dockerTag`).
- .NET had tests and no `--test` filter.
- Four extensions with a `workspace-install` declared no install inputs, so a
  lockfile change never made their install stale.
- Rust's `package` took no `--dry-run`, where the six others did.

## Decision

- **Coverage.** A coverage report that only the project's own setup produces,
  Coverlet's in .NET or SimpleCov's in Ruby, is optional and declares
  `warnWhenMissing`: when the test task writes none, the core reports the
  warning `stacks.coverage_unavailable` and the summary carries no coverage.
  A report that is optional for another reason, such as a Gradle build's
  JaCoCo report or a Bazel coverage file, stays silent. A coverage report the
  toolchain produces stays required; a toolchain that may lack coverage
  support declares `withoutCoverage`, as Rust and PHP do.
- **Image port.** `package --docker` takes `--port`, 8080 by default, in every
  stack, and the image sets `PORT` from it.
- **Dry run.** `package` and `publish` take `--dry-run` in every stack:
  `package` writes the channel record and builds nothing, as Java's does;
  Rust's also checks the platforms its flags name, and still stages a crate,
  which `cargo publish --dry-run` checks.
- **Run and serve.** `serve` takes `--port`, 3000 by default and 0 for a free
  port. `run` and `serve` take `--args`, the words passed to the program,
  except PHP's `serve`, whose built-in server takes no program arguments.
  `--entrypoint` names the program where a package holds several: Rust,
  Swift, Dart, Ruby and PHP; a Java or .NET component is one application.
- **Options.** Every flag of `package` and `publish` is read in its kebab-case
  and its camelCase form.
- **Test filter.** `test --test`, short `-t`, filters the tests in every stack
  whose runner can pass a run that selects no test. Dart's cannot: a filter
  that reaches the packages a selected one depends on would fail them.
- **Install inputs.** An extension with a `workspace-install` declares its
  root lockfiles as install inputs: `Cargo.lock`, `gradle.lockfile` and
  `settings-gradle.lockfile`, `Package.resolved`, `pubspec.lock`,
  `packages.lock.json`, `Gemfile.lock`, `composer.lock`.
- The conformance kit holds every stack extension's manifest to this surface,
  with a named exemption for each stated reason, so a drift fails the
  extension's own gate.

## Consequences

- A .NET project with no Coverlet collector, or a Ruby bundle with no
  SimpleCov, runs its tests and warns instead of failing.
- A Rust image listens on 8080 unless the project names another port.
- A new stack extension passes the kit only with the shared surface, or with
  an exemption its own tests state.
