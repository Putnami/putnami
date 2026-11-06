# Gradle and Maven repositories

`putnami observe` reads a Java or Kotlin repository built with Gradle or
Maven, Android included, without a Putnami workspace. It never runs Gradle or
Maven: it reads the settings, the build files and the POMs as text, and the
reports the CI already wrote.

## Gradle

A repository whose root holds `settings.gradle.kts` or `settings.gradle` is a
Gradle build. Each project the settings name in an `include` call with string
literals is a component of source `workspace` and ecosystem `gradle`, named by
its Gradle path:

```kotlin
rootProject.name = "shop"
include(":app", ":api", ":pricing")
include(":libs:core")
project(":legacy").projectDir = file("old/legacy")
```

`:app` is the directory `app` and `:libs:core` is `libs/core`. A
`projectDir` set to `file("dir")` or `File(settingsDir, "dir")` moves the
project; `File(rootDir, "dir")` and `File(rootProject.projectDir, "dir")` read
the same way. The root project is a component only when the root holds a
`src` directory, as a single-project build does. An Android module, of
`com.android.application` or `com.android.library`, is a component like any
other: the detector does not read plugins.

The edges come from each project's `build.gradle.kts` or `build.gradle`,
outside comments:

| Reference | Names |
|---|---|
| `project(":pricing")`, `project(path: ":pricing")`, `project(path = ":pricing")` | `:pricing` |
| `project("impl")` in `:libs:core` | `:libs:core:impl`: a relative path is relative to the project |
| `projects.pricing` | `:pricing`, the type-safe project accessor |
| `projects.libs.sharedModel` | `:libs:shared-model`: kebab and snake case become camel case |

A reference to a project the settings do not include is no edge.

The read is static. An `include` argument or a `projectDir` value that is not
a string literal, such as a variable, a template or a loop over directories,
gets `observe.gradle_dynamic_settings`, and the projects the settings name with
literals stay components:

```kotlin
file("plugins").listFiles()?.forEach { include(":plugins:${it.name}") }
```

Composite builds (`includeBuild`), `buildSrc`, convention plugins and the
dependencies a `subprojects` or `allprojects` block adds are not read.

## Maven

A repository whose root holds `pom.xml` is a Maven build. The `<modules>` a
POM lists, or the `<subprojects>` of Maven 4, in the project and in its
profiles, are read recursively: each from the `pom.xml` of its directory, or
from the POM file the entry names. Each POM whose `<packaging>` is not `pom`
is a component of source `workspace` and ecosystem `maven`, named
`groupId:artifactId`, with the `groupId` of its `<parent>` when it states
none. An aggregator, of packaging `pom`, is no component.

A `<dependency>` of the project or of one of its profiles whose `groupId` and
`artifactId` name another module of the repository is an edge.
`${project.groupId}`, `${pom.groupId}`, `${project.parent.groupId}` and the
POM's own `<properties>` are resolved first. A dependency of
`<dependencyManagement>` or of a plugin is no edge. A parent POM outside the
repository and a BOM are not read.

## Reports

Discovery finds the reports by their content. It walks every directory but
skips the build outputs that hold no report: `build/intermediates`,
`build/generated`, `build/tmp` and `build/kotlin` of Gradle and Android, and
`target/classes`, `target/test-classes`, `target/generated-sources` and
`target/generated-test-sources` of Maven.

| Tool | JUnit | Coverage |
|---|---|---|
| Gradle | `build/test-results/<task>/TEST-*.xml` | `build/reports/jacoco/<task>/*.xml`, Kover's `build/reports/kover/report.xml` |
| Android | `build/test-results/testDebugUnitTest/`, `build/outputs/androidTest-results/` | `build/reports/coverage/` |
| Maven | `target/surefire-reports/TEST-*.xml`, `target/failsafe-reports/TEST-*.xml` | `target/site/jacoco/jacoco.xml` |

A JUnit case whose class name, without its `$` nested class, names a `.java`
or `.kt` file under `src/test/java`, `src/test/kotlin`, `src/androidTest/java`
or `src/androidTest/kotlin` goes to the component of that file:
`com.acme.pricing.PriceTest$Discounts` is
`src/test/kotlin/com/acme/pricing/PriceTest.kt`. The report's own component is
tried first, then every component, so a report gathered at the root lands too.

A JaCoCo report names each file by its package path and source file name,
`com/acme/pricing/Price.kt`. The file maps to the first component whose
`src/main/java` or `src/main/kotlin` holds it, the report's own component
first. JaCoCo records which instructions of a line ran, not how often: a
covered line counts one hit. Kover writes the same format. Other source roots,
such as Kotlin Multiplatform's `src/commonMain/kotlin`, are not read.

## Dependency changes

| File | Packages |
|---|---|
| `gradle.lockfile`, `settings-gradle.lockfile` | each `group:artifact:version=…` line, as `group:artifact` |
| a version catalog, `gradle/libs.versions.toml` or another `.versions.toml` file of a `gradle` directory | each library as `group:artifact`, each plugin as its id |

Both are dependency changes of ecosystem `maven`. A catalog entry takes the
version it states, the one its `version.ref` names in `[versions]`, or the
`strictly`, `require` or `prefer` member of a rich version. An entry without a
version, such as a library a platform manages, is no package. A `pkg:maven`
package URL of an SBOM matches a change by `group:artifact`.

A Maven build has no lockfile, so it has no dependency change. An SBOM the CI
writes, with `cyclonedx-maven-plugin`, gives the licences and known
vulnerabilities of a lockfile change, not the change itself.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.gradle_dynamic_settings` | The settings compute an include or a `projectDir`; the projects named with literals stay components. |
| `observe.gradle_project_missing` | An included project has no directory, or lies outside the repository. |
| `observe.gradle_settings_unreadable` | The settings file cannot be read. |
| `observe.maven_module_missing` | A listed module has no POM, or lies outside the repository. |
| `observe.maven_pom_invalid` | A POM does not parse or states no `artifactId`; its modules are not read. |
| `observe.maven_walk_bounded` | The detector stopped after 10,000 POMs. |
