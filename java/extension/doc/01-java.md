# Run a Gradle or Maven build with Putnami

`@putnami/java` turns each Gradle project, Android included, and each Maven
module of a Java or Kotlin repository into a Putnami project, and runs its
lint, test and build with the build the team already uses. It reads the
repository the way `putnami observe` does, and it never installs a JDK, Gradle
or Maven.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/java"] }
   ```

2. Run `putnami projects sync`. Each Gradle project that `settings.gradle.kts`
   or `settings.gradle` includes with a string literal, the root project when
   it has a `src/` directory, and each module the root `pom.xml` lists, with
   any packaging but `pom`, joins the workspace's `includes`. No `putnami.json`
   is needed.
3. Run `putnami lint,test,build`.

| Component | Project name | `{component}`, the build's reference |
|---|---|---|
| Gradle `:libs:core` | `libs/core` | `:libs:core:`, the task path prefix |
| Gradle root project | its directory's name | `:` |
| Maven `com.acme:pricing` | `pricing` | `com.acme:pricing` |

Each project gets an edge to each component it depends on in the same build:
`project(":x")` and `projects.x` in a Gradle build file, a dependency on a
sibling module's coordinates in a POM. A directory that is both a Gradle
project and a Maven module runs as the Gradle project, with the warning
`java.build_conflict`.

## The variant of each component

The extension reads each component's build file and gives it a variant: a
part of the profile with that build's steps and reports.

| Variant | When |
|---|---|
| `gradle` | A Gradle project, without the ones below |
| `gradle-jacoco` | Its build file applies the `jacoco` plugin |
| `android` | Its build file applies an Android plugin (`com.android.application`, `com.android.library`, a `libs.plugins.android.*` alias) or holds an `android { }` block |
| `android-coverage` | An Android project whose build file turns on `enableUnitTestCoverage` |
| `maven` | A Maven module |

## What each task runs

Each task runs in the component's directory, with no shell. Gradle runs
through the wrapper at the build root, `{root}/gradlew`, with the component's
task path; Maven runs `mvn` with `--also-make`, so the modules a component
depends on are built in the same reactor.

| Variant | `lint` | `test` | `build` |
|---|---|---|---|
| `gradle` | `gradlew {component}check -x {component}test` | `gradlew {component}test` | `gradlew {component}assemble` |
| `gradle-jacoco` | as `gradle` | `gradlew {component}test {component}jacocoTestReport` | as `gradle` |
| `android` | as `gradle` | `gradlew {component}testDebugUnitTest` | `gradlew {component}assembleDebug` |
| `android-coverage` | as `gradle` | `gradlew {component}testDebugUnitTest {component}createDebugUnitTestCoverageReport` | as `android` |
| `maven` | `mvn --projects {component} --also-make verify -DskipTests` | `mvn --projects {component} --also-make verify` | `mvn --projects {component} --also-make package -DskipTests` |

Gradle commands also pass `--console=plain`, and Maven ones `--batch-mode` and
`--file {root}/pom.xml`, so the reactor is the whole build.

Maven `lint` runs `verify` without the tests, so the checks a POM binds to
`validate` or `verify` (Checkstyle, Spotless, PMD, SpotBugs) run. Maven `test`
runs `verify`, so the JaCoCo report a POM binds to `verify` is written. With
`--also-make`, the tests of the modules a component depends on run again in
its `test`; a repository that installs its modules into the local repository
can drop the flag with an override.

## Reports

The test task reads the JUnit reports and the JaCoCo XML report into the test
and coverage summary.

| Variant | JUnit | JaCoCo |
|---|---|---|
| `gradle` | `build/test-results/test/*.xml` | none |
| `gradle-jacoco` | `build/test-results/test/*.xml` | `build/reports/jacoco/test/jacocoTestReport.xml` |
| `android` | `build/test-results/testDebugUnitTest/*.xml` | none |
| `android-coverage` | `build/test-results/testDebugUnitTest/*.xml` | `build/reports/coverage/test/debug/report.xml` |
| `maven` | `target/surefire-reports/TEST-*.xml`, `target/failsafe-reports/TEST-*.xml` | `target/site/jacoco/jacoco.xml`, when the build writes one |

A component with no file under `src/test` (or `src/testDebug` for Android)
expects no report, so a module without tests passes. Otherwise a failed case,
a step that exits non-zero, or a JUnit report that is missing fails the task,
and so does a missing JaCoCo report for `gradle-jacoco` and
`android-coverage`. The reports are removed before the steps, so a report an
earlier run left is never read as this run's.

## The toolchain check

Before its steps, each task compares the version `java -version` prints with
the one `.java-version` states, in the component's directory, then at the
workspace root.

| Case | Result |
|---|---|
| The versions match | The task runs; `21` matches every 21 release, such as 21.0.4, and keys the major line |
| They differ | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| `java` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| No `.java-version` | The warning `stacks.toolchain_unpinned`: the cache key carries no Java version |

The Gradle wrapper downloads the Gradle version
`gradle/wrapper/gradle-wrapper.properties` names; that file is an input of
every Gradle task, so a wrapper upgrade reruns them. A build that uses Gradle
toolchains (`jvmToolchain(21)`) compiles with the JDK it names, whatever runs
Gradle. Java 8's `1.8.0_392` form matches only an exact `.java-version`.

## What reaches the cache key

A task's key covers the component's files its task reads, the build-root
files, the component's `putnami.json`, its variant and the expected Java
version, and the key of the same task of each component it depends on.

| Input set | Patterns |
|---|---|
| `sources` | `src/main/**`; for Android also `src/debug/**`, `src/release/**`, `proguard-rules.pro`, `consumer-rules.pro` |
| `tests` | `src/test/**`, `src/testFixtures/**`; for Android also `src/testDebug/**` |
| `gradle` | the build file and `gradle.properties` of the project; at the root the settings and build files, `gradle.properties`, `gradle/libs.versions.toml`, the wrapper properties, `buildSrc/**` and `build-logic/**` |
| `gradleLocks` | `gradle.lockfile`; at the root `settings-gradle.lockfile` and `gradle.lockfile` |
| `maven` | `pom.xml`; at the root `pom.xml` and `.mvn/maven.config`, `.mvn/jvm.config`, `.mvn/extensions.xml` |
| `lintConfig` | `lint.xml`, `detekt.yml`; at the root `.editorconfig` and `config/checkstyle/**`, `config/detekt/**`, `config/pmd/**`, `config/spotbugs/**` |

`lint` reads `sources`, the build set and `lintConfig`; `test` reads
`sources`, `tests`, the build set and the lockfiles; `build` reads `sources`,
the build set and the lockfiles. So a change under `src/test` or to a
lockfile reruns test and build, never lint.

## Override the profile

A component overrides any value of its variant's profile in its own
`putnami.json`, under `options["@putnami/java"]`. An object merges; any other
value, such as a list of steps or patterns, replaces the variant's whole. The
overrides a team uses most:

An Android app with product flavors runs the unit tests of one flavor:

```json
{
  "options": {
    "@putnami/java": {
      "tasks": {
        "test": {
          "steps": [["{root}/gradlew", "--console=plain", "{component}testFreeDebugUnitTest"]],
          "reports": { "junit": ["build/test-results/testFreeDebugUnitTest/*.xml"], "coverage": [] }
        },
        "build": { "steps": [["{root}/gradlew", "--console=plain", "{component}assembleFreeDebug"]] }
      },
      "inputs": { "sources": ["src/main/**", "src/free/**", "src/debug/**"] }
    }
  }
}
```

A Maven repository with the Maven wrapper runs it instead of `mvn`:

```json
{
  "options": {
    "@putnami/java": {
      "tasks": {
        "test": { "steps": [["{root}/mvnw", "--batch-mode", "--file", "{root}/pom.xml", "--projects", "{component}", "--also-make", "verify"]] }
      }
    }
  }
}
```

A Gradle lint that also checks the test sources, such as `checkstyleTest` or
detekt on `src/test`, reads them:

```json
{ "options": { "@putnami/java": { "tasks": { "lint": { "inputs": ["sources", "tests", "gradle", "lintConfig"] } } } } }
```

A JDK that differs from `.java-version` on purpose:

```json
{ "options": { "@putnami/java": { "toolchain": { "check": "warn" } } } }
```

A steps override keeps the variant's reports: a test step must still write
them where the variant reads them, or the override also replaces
`tasks.test.reports`. An override fails the task with
`stacks.project_config_invalid` when it names an unknown member, changes `id`,
`markers` or `variants`, or adds a build-root input the extension does not
declare. The design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- The build root is the workspace root: a Gradle or Maven build in a
  subdirectory, and a Gradle composite build (`includeBuild`), are not read.
- A Gradle settings file that computes its includes keeps the projects it
  names with string literals, with the warning `observe.gradle_dynamic_settings`.
- A plugin applied from a convention plugin or from the root build file, such
  as `subprojects { apply(plugin = "jacoco") }`, picks no variant: the
  project overrides its test task.
- Gradle builds without a wrapper: override the steps to run `gradle`.
- A Maven parent POM between the root and a module is not an input of the
  module's tasks.
- No publish: an artifact reaches its repository through Gradle or Maven.
