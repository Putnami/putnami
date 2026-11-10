# ADR 0003 — A test filter reaches Gradle through an init script

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/java`, the `test` command's `--test` flag

## Context

`putnami test --test <filter>` hands the filter to the test task of each
selected component and of each component they depend on, where it may select
no test. The stack core appends a filter's arguments to the end of the task's
last step.

- Gradle's `--tests` is an option of the task right before it. The steps of
  `gradle-jacoco` and `android-coverage` end with their report task, so an
  appended `--tests` would reach `jacocoTestReport` or
  `createDebugUnitTestCoverageReport`, which have no such option. A test task
  in which `--tests` selects nothing fails the build, and
  `filter.failOnNoMatchingTests` has no command-line flag.
- Maven's Surefire reads `-Dtest`, and `-Dsurefire.failIfNoSpecifiedTests=false`
  passes a module in which it selects nothing. `-Dtest` replaces Surefire's own
  includes, and Failsafe's `-Dit.test` replaces Failsafe's: with Surefire and
  Failsafe 3.5.2, `-Dit.test=QuotesTest` ran the unit test `QuotesTest` again
  in Failsafe, and `-Dtest=QuotesIT` ran the integration test in Surefire while
  Failsafe ran every integration test.
- Neither build writes a JUnit report for a test task that runs no test, and
  the core fails a test task that finds no report with
  `stacks.report_missing`.

## Decision

- For a last step that runs `gradlew` or `gradle`, the extension writes an
  init script, `putnami-test-filter.gradle`, into the task's output directory,
  and appends `--init-script <script> -Dputnami.testFilter=<filter>`. Both are
  global options, which Gradle reads wherever they stand. The script reads the
  property with `providers.systemProperty`, and gives every test task
  (`AbstractTestTask`) the filter with `setTestNameIncludePatterns`, the
  setter `--tests` calls, and `failOnNoMatchingTests = false`. A task then
  runs the tests both its own filter and this one select.
- For a last step that runs `mvn`, `mvnw` or `mvnd`, the extension appends
  `-Dtest=<filter> -Dsurefire.failIfNoSpecifiedTests=false -DskipITs`.
  Failsafe gets no filter and runs no integration test.
- The job runs through the core's `RunFiltered`: a filtered run whose steps
  pass and that finds no JUnit report reads zero tests and no coverage.
- A last step that runs any other program, which an override names, gets no
  filter. The filter is a parameter input of the test task, so a filtered
  result never answers an unfiltered run.

## Rejected alternatives

- **`--tests` inserted after the test task.** It needs the extension to know
  which argument of an overridden step is the test task, and still fails a
  component in which the filter selects no test.
- **The filter written into the init script's text.** It must then be escaped
  as Groovy, and a filter is user text. A system property carries it as data.
- **`filter.includeTestsMatching` in the script.** It adds to the patterns a
  build sets, so a build that includes only `*UnitTest` would also run the
  other tests the filter names; `--tests` narrows the build's own filter.
- **`-Dit.test=<filter>` for Failsafe.** It runs each unit test the filter
  names a second time, and the summary, which reads both report directories,
  counts it twice.
- **Failsafe's integration tests run whole.** The filter would no longer
  narrow the run, and an integration test it names would run in Surefire and
  again in Failsafe.

## Consequences

- With the configuration cache on, Gradle reuses its entry for the same
  filter and configures again for another one or for none: the property and
  the init script are inputs of its entry. Checked with Gradle 8.14.3 and
  9.9.0 on a build with the configuration cache on.
- A filtered Maven run runs no Failsafe integration test, and an integration
  test the filter names runs in Surefire's test phase, without Failsafe's pre-
  and post-integration-test phases.
- A Gradle filter follows `--tests` syntax, a Maven one Surefire's `-Dtest`
  syntax: a filter shared by both builds names a class by its simple or
  qualified name.
