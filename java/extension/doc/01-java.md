# Run a Gradle or Maven build with Putnami

`@putnami/java` turns each Gradle project, Android included, and each Maven
module of a Java or Kotlin repository into a Putnami project, and runs its
lint, test and build with the build the team already uses. It reads the
repository the way `putnami observe` does. A machine with no JDK, no Gradle
and no Maven builds it right after `git clone`: `putnami install` installs the
pinned Eclipse Temurin JDKs the build asks for, its Gradle or Maven, and its
dependencies, and each task installs what it still lacks. An application also
serves, runs, and ships as an image. The samples in
[`java/samples`](https://github.com/Putnami/putnami/blob/main/java/samples/README.md)
show a Gradle build with and without a wrapper and a Maven build.

## Adopt it

`@putnami/java` is not published yet: until its first release, `putnami init
--extension java` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

1. At the root of the repository, run:

   ```bash
   putnami init --extension java
   ```

   It writes `putnami.workspace.json` with `@putnami/java` declared, installs
   the extension, and runs `putnami projects sync`, because the repository
   holds a Gradle settings or build file or a `pom.xml`. A repository of several languages takes a list, such as
   `--extension java,typescript`, and `putnami init` without `--extension`
   proposes the languages it finds. In a workspace that already exists,
   declare the extension in `putnami.workspace.json` and run
   `putnami projects sync`:

   ```json
   { "extensions": ["@putnami/java"] }
   ```

2. Each Gradle project that `settings.gradle.kts`
   or `settings.gradle` includes with a string literal, the root project when
   it has a `src/` directory, and each module a `pom.xml` lists, with any
   packaging but `pom`, joins the workspace's `includes`. No `putnami.json` is
   needed.
3. Run `putnami install` to prepare the machine
   ([Prepare a machine](#prepare-a-machine)), then
   `putnami lint,test,build`.

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

A component is an `application` when its build file applies the Gradle
`application` or Spring Boot plugin, in a `plugins {}` block or with
`apply plugin`, or its POM's build applies `spring-boot-maven-plugin` with no
`<skip>true</skip>`. Any other is a `library`, which `serve`, `run` and
`package` leave out: a dependency on Spring Boot makes no application, and an
Android application, which runs on a device, is a library to Putnami.

### Builds in subdirectories

A build need not sit at the workspace root. A Gradle build's root is the
nearest directory that holds `settings.gradle.kts` or `settings.gradle`; a
Maven build's root is the outermost `pom.xml` that lists the module, through a
chain of `pom.xml` files or a `<module>` path such as `services/api`, else the
module's own. That root is the stack root of the build's components: their steps run
its `gradlew` or its `pom.xml`, and the tasks of one build run one at a time.
When the root is no component itself, it is an input-only project its
components depend on, so its settings, wrapper and root POM reach their keys.
A Gradle build inside another one, such as `buildSrc` or an included build, is
part of the build that holds it, and a nested Gradle build the outer one does
not include is left out. A build file under the `src`, `target` or `build`
directory of another build, such as an invoker test or an archetype template,
is no build; a build under a plain `src` directory, such as `src/adservice`,
is. A Gradle plugin declared with `apply false` is not applied.

## The variant of each component

The extension reads each component's build file and gives it a variant: a
part of the profile with that build's steps and reports.

| Variant | When |
|---|---|
| `gradle` | A Gradle project, without the ones below |
| `gradle-jacoco` | Its build file applies the `jacoco` plugin |
| `gradle-platform` | Its build file applies `java-platform`: a project with `check` and `assemble` and no test task |
| `gradle-catalog` | Its build file applies `version-catalog`: a project with neither |
| `android` | Its build file applies an Android plugin (`com.android.application`, `com.android.library`, a `libs.plugins.android.*` alias) or holds an `android { }` block |
| `android-coverage` | An Android project whose build file turns on `enableUnitTestCoverage` (or the older `testCoverageEnabled`) |
| `maven` | A Maven module |

## What each task runs

Each task runs in the component's directory, with no shell. Gradle runs
through the wrapper at the build root, `{root}/gradlew`, with the component's
task path, or through a pinned Gradle when the build has no wrapper; Maven runs
`mvn` with `--also-make`, so the modules a component depends on are built in
the same reactor. The `mvn` is the release the build's Maven wrapper states,
else the one on `PATH`, else a pinned Maven
([A machine with no JDK, Gradle or Maven](#a-machine-with-no-jdk-gradle-or-maven)).

| Variant | `lint` | `test` | `build` |
|---|---|---|---|
| `gradle` | `gradlew {component}check -x {component}test` | `gradlew {component}test` | `gradlew {component}assemble` |
| `gradle-jacoco` | as `gradle` | `gradlew {component}test {component}jacocoTestReport` | as `gradle` |
| `gradle-platform` | `gradlew {component}check` | none | `gradlew {component}assemble` |
| `gradle-catalog` | none | none | `gradlew {component}generateCatalogAsToml` |
| `android` | as `gradle` | `gradlew {component}testDebugUnitTest` | `gradlew {component}assembleDebug` |
| `android-coverage` | as `gradle` | `gradlew {component}testDebugUnitTest {component}createDebugUnitTestCoverageReport` | as `android` |
| `maven` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make verify -DskipTests` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make verify` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make package -DskipTests` |

Gradle commands also pass `--console=plain`, and Maven ones `--batch-mode` and
`--file {root}/pom.xml`, so the reactor is the whole build.
Maven first cleans the component alone: its compiler recompiles a module only
when a dependency changed in the same run, so a module whose upstream changed
in an earlier task would otherwise pass on its old classes. Only the
component's own `target/` is removed, not the root's, which a Cargo
workspace at the root may share, unless the component is the root module of
a single-module build. The clean also removes what an earlier task
of the component left in `target/`, such as the jar of a `build`; the reports
a task reads are copied out before it ends, so a cache hit restores them.

Maven `lint` runs `verify` without running the tests, so the checks a POM binds to
`validate` or `verify` (Checkstyle, Spotless, PMD, SpotBugs) run. Maven `test`
runs `verify`, so the JaCoCo report a POM binds to `verify` is written. With
`--also-make`, the tests of the modules a component depends on run again in
its `test`; a repository that installs its modules into the local repository
can drop the flag with an override.

## Test filter and documentation links

`putnami test --test <filter>`, short `-t`, runs the tests the filter names.
The filter also reaches the components the selected ones depend on, where it
may select no test: such a component passes, with zero tests and no coverage.
The filter is part of the task's key, so a filtered result never answers an
unfiltered run.

| Build | The filter |
|---|---|
| Gradle | Each test task runs it as `--tests <filter>` would, such as `com.acme.QuotesTest` or `*QuotesTest.labels`: the tests both the task's own filter and this one select. A task in which it selects no test passes |
| Maven | `-Dtest=<filter> -Dsurefire.failIfNoSpecifiedTests=false -DskipITs` on the `verify` step: Surefire runs the tests it names, such as `QuotesTest#labels`, a module in which it selects none passes, and Failsafe runs no integration test |

`--tests` is an option of the task right before it, and the `gradle-jacoco`
and `android-coverage` steps end with their report task. So the extension
writes an init script, `putnami-test-filter.gradle`, into the task's output
directory, and appends `--init-script <script> -Dputnami.testFilter=<filter>`:
global options, which Gradle reads wherever they stand. The script gives every
test task the filter and turns off `failOnNoMatchingTests`. It reads the
property through the provider API, so a build with the configuration cache on
reuses its entry for the same filter and configures again for another one.

Failsafe gets no filter: `-Dit.test` replaces its own includes, so it would
run again, in Failsafe, each unit test the filter names, and the summary,
which reads both report directories, would count it twice. A filtered run
skips Failsafe instead, so the filter narrows the run as on Gradle, whose test
step runs no integration test task. A filter that names an integration test
runs it in Surefire's test phase, without Failsafe's pre- and
post-integration-test phases. The choices are in
[ADR 0003](https://github.com/Putnami/putnami/blob/main/java/extension/doc/adr/0003-a-test-filter-reaches-gradle-through-an-init-script.md).

A step override keeps the filter when its last step runs `gradlew`, `gradle`,
`mvn`, `mvnw` or `mvnd`; a last step that runs any other program gets none.

`putnami lint` also checks every relative link and anchor of the component's
`README.md` files and `doc/` trees, and a broken one fails it. A directory
below the component that holds a build file (`build.gradle.kts`,
`build.gradle`, `pom.xml`, `settings.gradle.kts` or `settings.gradle`) is a
component or a build of its own, which answers for its own documents.
`--docs-links=false` turns the check off. The check needs no JDK.

## Reports

The test task reads the JUnit reports and the JaCoCo XML report into the test
and coverage summary.

| Variant | JUnit | JaCoCo |
|---|---|---|
| `gradle` | `build/test-results/test/*.xml` | none |
| `gradle-jacoco` | `build/test-results/test/*.xml` | `build/reports/jacoco/test/jacocoTestReport.xml`, when the build turns on its XML report (`reports { xml.required = true }`) |
| `android` | `build/test-results/testDebugUnitTest/*.xml` | none |
| `android-coverage` | `build/test-results/testDebugUnitTest/*.xml` | `build/reports/coverage/test/debug/report.xml` |
| `maven` | `target/surefire-reports/TEST-*.xml`, `target/failsafe-reports/TEST-*.xml` | `target/site/jacoco/jacoco.xml`, when the build writes one |

A component with no file under `src/test` (or `src/testDebug` for Android)
expects no report, so a module without tests passes. Otherwise a failed case,
a step that exits non-zero, or a JUnit report that is missing fails the task,
and so does a missing JaCoCo report for `android-coverage`, except in a run
the test filter selects no test in, which reads zero tests and no coverage.
The reports are removed before the steps, so a report an
earlier run left is never read as this run's.

## The toolchain check

Before its steps, each task compares the version `javac -version` prints with
the one `.java-version` states, in the component's directory, then at the
stack root. It checks `javac`, so a JRE alone is no JDK. `.java-version` is
read up to its first character that is neither a digit nor a dot:
`21.0.13+9` states `21.0.13`.

| Case | Result |
|---|---|
| The versions match | The task runs; `21` matches every 21 release, such as 21.0.4, and keys the major line |
| `javac` is missing, or differs | The task installs the JDK `.java-version` accepts, below, and runs it |
| No pinned JDK matches, such as `.java-version` 23 | The task fails with `stacks.toolchain_mismatch` or `stacks.toolchain_missing`; set `toolchain.check` to `warn` to run the machine's anyway |
| No `.java-version` | The warning `stacks.toolchain_unpinned`: the cache key carries no Java version |

Java 8 is both 8 and 1.8: its `javac` prints `1.8.0_512`, which `8`, `1.8`,
`1.8.0` and `8.0.512` accept. `.java-version` `1.8.0_512` states `1.8.0`, so
its update number is not compared.

### A machine with no JDK, Gradle or Maven

The extension pins five Eclipse Temurin JDKs, each by the SHA-256 the
Adoptium API publishes:

| JDK | Platforms |
|---|---|
| 1.8.0_512 (8u512) | Linux on x64 and arm64, Windows on x64: Adoptium publishes no current Java 8 for macOS |
| 11.0.33, 17.0.21, 21.0.13, 25.0.5 | Linux and macOS on x64 and arm64, Windows on x64 |

When `javac` is missing or outside `.java-version`, the task installs the one
`.java-version` accepts, 21 first, under `toolchains/java` of the Putnami
home, and runs it first on `PATH` and as `JAVA_HOME`, which Gradle and Maven
read. Every later task reuses it. The archive's links, license notices under
`legal/`, are not installed.

A build that uses Gradle toolchains compiles and tests with the JDK each one
asks for, whatever JDK runs Gradle. Gradle finds such a JDK among the ones it
detects on the machine, or downloads one through a toolchain resolver plugin
the build applies, such as foojay. So each Gradle step reads the Java versions
the build asks for and passes the pinned JDK of each to Gradle as
`-Porg.gradle.java.installations.paths=<homes>`, installing it first:

- `JavaLanguageVersion.of(17)` and `jvmToolchain(17)`, in the build files of
  the build's projects and root, in its script plugins under `gradle/`, and in
  the build files and sources of `buildSrc` and of every project of
  `build-logic`, convention plugins included;
- the version written as a number, a string, a `JavaVersion` constant such as
  `JavaVersion.VERSION_17`, or a version of the `libs` catalog,
  `libs.versions.jdk.get()`, read from `gradle/libs.versions.toml`;
- the daemon JVM criteria, `toolchainVersion` in
  `gradle/gradle-daemon-jvm.properties`.

The major of the JDK that runs Gradle needs no home: Gradle uses its own JVM.
A major the extension pins no JDK of is left to Gradle, with a message. Gradle
reads the property from 6.7 on; the extension's checks ran it on Gradle 7.6,
8.0, 8.14 and 9.8. Every Gradle step of a build gets the JDKs of the whole
build, since a task of one project compiles the projects it depends on.

A Gradle build with a wrapper runs it, and the wrapper downloads the Gradle
version `gradle/wrapper/gradle-wrapper.properties` names; that file is an input
of every Gradle task, so a wrapper upgrade reruns them. A build without a
wrapper, whose steps name `{root}/gradlew`, and a step that names `gradle` when
no `gradle` is on `PATH`, run a pinned Gradle distribution, checked against the
SHA-256 services.gradle.org publishes and installed under `toolchains/gradle`:

| JDK that runs Gradle | Gradle |
|---|---|
| 17 to 25 | 9.8.2 |
| 8 to 16 | 8.14.6 |

A Maven step runs the release the build's
`.mvn/wrapper/maven-wrapper.properties` names in its `distributionUrl`,
whatever `mvn` the machine has, installed under `toolchains/maven`. The archive
is checked against the `distributionSha256Sum` the file states, else against
the SHA-512, then the SHA-256, its host publishes beside it, such as
`apache-maven-3.9.16-bin.zip.sha512` on Maven Central; an archive whose host
publishes neither is refused. The extension never runs the `mvnw` script. A
build without the file runs the `mvn` on `PATH`, else Apache Maven 3.9.11,
installed the same way.

Offline, a task installs nothing: it runs what an earlier task or
`putnami install` installed, leaves out a toolchain JDK that is not installed,
and fails when the JDK, Gradle or Maven it runs is missing.
[ADR 0004](https://github.com/Putnami/putnami/blob/main/java/extension/doc/adr/0004-a-blank-machine-builds-after-putnami-install.md) records
these choices.

### An Android build

The Android Gradle plugin reads the Android SDK from `ANDROID_HOME`,
`ANDROID_SDK_ROOT` or `sdk.dir` in the build's `local.properties`, and
downloads the platforms and build tools the build declares only into an SDK
whose licenses are accepted. A machine that names an SDK keeps it. On a
machine that names none, the extension installs one only when the workspace's
owner accepts the Android SDK licenses on the team's behalf, in
`putnami.workspace.json`:

```json
{
  "options": {
    "@putnami/java": {
      "android": { "acceptSdkLicenses": true }
    }
  }
}
```

With the option, a task of a Gradle build that applies the Android plugin, in
the build file of its root or of one of its projects, and `putnami install`,
install the Android SDK command-line tools 22.0 under `toolchains/android` of
the Putnami home, checked by SHA-256, for Linux on x64, macOS on x64 and
arm64, and Windows on x64. They accept the licenses with
`sdkmanager --licenses`, run on the pinned JDK 21, into the SDK
`toolchains/android/sdk`, which every build of the machine shares, and run
the task with it as `ANDROID_HOME` and `ANDROID_SDK_ROOT`. The Android Gradle
plugin then downloads the platforms and build tools on the first task that
needs them. The extension pins 22.0 because from 23.0 `sdkmanager` hands over
to the Android CLI, which downloads itself unchecked and accepts no license.

Without the option, a task of an Android component on a machine with no SDK
fails with `stacks.toolchain_missing`, and the message names the option; a
task of another project of the build is left to Gradle. Offline, a task
installs nothing: it uses the SDK an earlier task or `putnami install`
installed.
[ADR 0005](https://github.com/Putnami/putnami/blob/main/java/extension/doc/adr/0005-an-android-build-gets-the-sdk-the-workspace-accepts.md)
records these choices.

## Prepare a machine

`putnami install` runs the extension's `workspace-install` once per stack root
of the workspace's Gradle and Maven builds, in path order, and leaves out the
projects whose tags `disable.tags` names. A change to `gradle.lockfile` or
`settings-gradle.lockfile` at the workspace root makes the next `putnami`
command run it again. It installs:

1. The JDK `.java-version` accepts, as a task installs it, and the pinned JDKs
   the build's Gradle toolchains ask for.
   An Android build whose workspace accepts the Android SDK licenses also
   gets the SDK's command-line tools and its accepted licenses.
2. The build tool: a Gradle build runs `gradlew --version`, so the wrapper
   downloads its distribution, or installs the pinned Gradle; a Maven build
   installs the Maven its wrapper states, or Maven 3.9.11 when the machine
   has no `mvn`.
3. The dependencies. A Gradle build runs its Gradle with an init script that
   gives every project a task resolving each of its resolvable
   configurations; a configuration it cannot resolve whole is a warning,
   `java.dependencies_incomplete`, and a build that does not configure fails
   the install with `java.install_failed`. A Maven build runs
   `maven-dependency-plugin:3.11.0:go-offline`, best effort: it downloads the
   dependencies and plugins the POMs declare, not the artifacts a plugin
   resolves only when it runs, such as Surefire's test framework provider,
   which the warning `java.dependencies_incomplete` names; a run that fails is
   that warning too.

`putnami install --force` passes `--refresh-dependencies` to Gradle and
`--update-snapshots` to Maven. With offline dependencies the install verifies
and installs nothing: a root whose JDK, Gradle wrapper distribution or Maven
is missing is left out with a warning, and the resolution runs with
`--offline`. The tasks install what the install did not, so a run that skips
it still builds.

## What reaches the cache key

A task's key covers the component's files its task reads, the build-root
files, the component's `putnami.json`, its variant and the expected Java
version, and the key of the same task of each component it depends on.

| Input set | Patterns |
|---|---|
| `sources` | `src/main/**`; for Android also `src/debug/**`, `src/release/**`, `proguard-rules.pro`, `consumer-rules.pro` |
| `tests` | `src/test/**`, `src/testFixtures/**`; for Android also `src/testDebug/**` |
| `gradle` | the build file and `gradle.properties` of the project; at the root the settings and build files, `gradle.properties`, the catalogs and script plugins under `gradle/`, each project's own `*.versions.toml` and `gradle/*.versions.toml`, the wrapper properties, `gradle/gradle-daemon-jvm.properties`, the build files and `src/**` of `buildSrc`, and the build files and `src/**` of every project of `build-logic` |
| `gradleLocks` | `gradle.lockfile`; at the root `settings-gradle.lockfile` and `gradle.lockfile` |
| `maven` | `pom.xml`; every `pom.xml` of the build up to four directories below its root, parents included, and `.mvn/maven.config`, `.mvn/jvm.config`, `.mvn/extensions.xml` and `.mvn/wrapper/maven-wrapper.properties` at the root. The copy a jar build writes under `target/classes/META-INF/maven` lies deeper and reaches no key |
| `lintConfig` | `lint.xml`, `detekt.yml`; at the root `.editorconfig` and `config/checkstyle/**`, `config/detekt/**`, `config/pmd/**`, `config/spotbugs/**` |

`lint` reads `sources`, `tests`, the build set and `lintConfig`: Gradle's
`check` compiles and checks the test sources (`checkstyleTest`, `pmdTest`,
detekt), and Maven's `-DskipTests` still compiles them and builds test-jars.
`test` reads `sources`, `tests`, the build set and the lockfiles; `build`
reads `sources`, the build set and the lockfiles, and Maven's also `tests`.
So a lockfile change reruns test and build, never lint. A change to any POM
reruns every Maven task: a parent POM's settings reach every module.

## Serve and run an application

`putnami serve` and `putnami run` first build the application's runnable form
with its own build, under the lock the tasks of its build share, copy it into
a private directory, and start the copy in the application's directory on the
task's JDK:

| Application | Built with | Started as |
|---|---|---|
| Gradle `application` plugin | `gradlew {component}installDist` | `build/install/<name>/bin/<name>` |
| Gradle with Spring Boot | `gradlew {component}bootJar` | `java -jar build/libs/<name>.jar` |
| Maven with Spring Boot | `mvn --projects {component} --also-make package -DskipTests` | `java -jar target/<artifact>.jar` |

`serve` sets `PORT` and `SERVER_PORT`, which Spring Boot reads, to its
`--port` flag, 3000 by default, and reports the server ready once the port
accepts connections, so `putnami qualify` requests the routes that
`schema/http-routes.json` lists. The words of `--args` reach the application.
It runs until the user stops it. `run` runs the application once with
`--args` and answers its exit code.

## Package and publish an image

`putnami package --docker` assembles the image of an application with the OCI
assembler of the extension SDK, with no Docker daemon: its runnable form under
`/app`, on the Eclipse Temurin JRE of the task's Java major
(`eclipse-temurin:<release>-jre`, pinned by digest for 17, 21 and 25), run as
user 1000 with `PORT` and `SERVER_PORT` set to `--port`, 8080 by default. The image name is the
project name in lowercase.

```bash
putnami package --docker --docker-load --projects /java/samples/maven/application
putnami publish --docker --projects /java/samples/maven/application
```

| Flag | Meaning |
|---|---|
| `--docker-base-image` | A base pinned by digest, `image@sha256:<digest>`, in place of the Temurin JRE |
| `--docker-tag` | A tag beside the content tag `c-<hash>` |
| `--docker-load` | Also load the image into the local Docker daemon |
| `--platform` | The image platform, `linux/amd64` by default |

An application turns the image on with `"options": { "package": { "docker":
true } }` in its `putnami.json`, or with `"publish": ["docker"]`, which also
declares the image as a member of the release set under the namespace of
`registries.oci.publish`. `putnami publish --docker` pushes it through the
publisher every stack shares, and `--docker-registry <registry>` overrides that
registry for one push. A library packages nothing: it reaches its Maven
repository through its own build.

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

A Maven repository whose wrapper script does more than name its release runs
the script instead of `mvn`:

```json
{
  "options": {
    "@putnami/java": {
      "tasks": {
        "test": { "steps": [
          ["{root}/mvnw", "--batch-mode", "--file", "{root}/pom.xml", "--projects", "{component}", "clean"],
          ["{root}/mvnw", "--batch-mode", "--file", "{root}/pom.xml", "--projects", "{component}", "--also-make", "verify"]
        ] }
      }
    }
  }
}
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
[its design note](https://github.com/Putnami/putnami/blob/main/tooling/stacks/doc/01-design.md).

## Limits

- A Gradle composite build (`includeBuild`) runs as the build that includes
  it: its included builds are not projects.
- A Gradle settings file that computes its includes keeps the projects it
  names with string literals, with the warning `observe.gradle_dynamic_settings`.
- A plugin applied from a convention plugin or from the root build file, such
  as `subprojects { apply(plugin = "jacoco") }`, picks no variant: the
  project overrides its test task.
- A Gradle toolchain version the build computes another way, such as from
  `gradle.properties` or a variable, is not read, and neither is a
  toolchain's vendor: Gradle finds or provisions such a JDK itself, and a
  toolchain that asks for a vendor other than Adoptium or Eclipse does not
  match the pinned JDKs.
- The build file of an intermediate Gradle project that configures its
  children, such as `libs/build.gradle.kts` for `:libs:core` with a
  `subprojects { }` block, is not in the children's keys: after a change to
  it, a cached result of a child can be stale. Move that configuration to a
  convention plugin under `buildSrc` or `build-logic`, which every key reads,
  or run the children's tasks with `--no-cache` after such a change.
- A Maven module is named by its artifactId: two modules with the same
  artifactId in different groups need a `name` in their `putnami.json`.
- The Maven wrapper `mvnw` is not used unless a project overrides its steps:
  the release its properties name runs instead.
- An application built another way, such as a fat jar from the Shadow plugin or
  a Maven module with no Spring Boot, is a library to `serve`, `run` and
  `package`.
- A Maven module is built with `--also-make` by `serve`, `run` and `package`,
  so the modules it depends on are built again.
- An image is built for one platform; the tests run the `linux/amd64` one.
- The image's JRE follows the JDK that ran the build, not a Gradle toolchain
  the build compiles for: a `jvmToolchain(25)` build run on JDK 21 names
  `--docker-base-image` with a Java 25 base.
- A JDK found only through `JAVA_HOME`, with no `javac` on `PATH`, is not
  seen: the task installs one.
- A library is published by its own build, not by `putnami publish`.
- There is no `workspace-fetch`, the job that fills a dependency cache ahead
  of a hosted run without running repository code: resolving a Gradle build's
  dependencies evaluates its settings and build scripts, which run the code of
  their plugins, through the wrapper jar the repository holds, and Maven's
  `dependency:go-offline` loads the build's extensions and misses the
  dependencies a plugin resolves only when it runs. `putnami install` runs
  that code on the machine it prepares, and Gradle and Maven download what
  the build needs when its tasks run, over the network the runner allows.
- The Android SDK is installed only once the workspace accepts its licenses,
  and Linux on arm64 gets none: Google publishes its build tools for x64
  only. The platforms and build tools are left to the Android Gradle plugin.
- Credentials for a private Maven or Gradle repository are the build's own:
  `~/.m2/settings.xml`, `~/.gradle/gradle.properties` or the environment the
  build reads. The extension passes none.
- There is no `deps-upgrade`: a Gradle or Maven build moves its versions with
  its own tools, such as a version catalog update plugin or the versions
  plugin of Maven.
