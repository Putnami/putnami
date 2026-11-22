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
| `project(":pricing")`, `project(path: ":pricing")`, `project(path = ":pricing")`, `findProject(":pricing")` | `:pricing` |
| `project("impl")` in `:libs:core` | `:libs:core:impl`: a relative path is relative to the project |
| `projects.pricing` | `:pricing`, the type-safe project accessor |
| `projects.libs.sharedModel` | `:libs:shared-model`: kebab and snake case become camel case |

A reference to a project the settings do not include is no edge.

The read is static. An `include` argument or a `projectDir` value that is not
a string literal, such as a variable, a template or a loop over directories,
gets `observe.gradle_dynamic_settings`, and the projects the settings name with
literals stay components. So does an `include` without parentheses or quotes,
such as Groovy's `include it`, and a function reference, `forEach(::include)`.
The word `include` inside a string or a comment is no include:

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
| each POM of a Maven build | the versions it declares, as `group:artifact`, marked declared |

All are dependency changes of ecosystem `maven`. A catalog entry takes the
version it states, the one its `version.ref` names in `[versions]`, or the
`strictly`, `require` or `prefer` member of a rich version. An entry without a
version, such as a library a platform manages, is no package. A `pkg:maven`
package URL of an SBOM matches a change by `group:artifact`.

Maven has no lockfile, so a Maven build's dependency changes are the versions
its POMs declare. Each POM the detector reads, the root `pom.xml` and the
modules it lists, and a module `pom.xml` the change deletes, is read at the
merge base and at the head. Its parent and each dependency of `dependencies`
and `dependencyManagement`, in the project and in its profiles, give their
`groupId:artifactId` at the version they state. `${project.version}`,
`${project.parent.version}`, `${project.groupId}`, `${project.parent.groupId}`
and the properties of the POM, of its profiles and of its parent POMs in the
repository are resolved, a property that names another one included; the
project's version and groupId default to its parent's. A BOM a
`dependencyManagement` entry imports is a package at its version.

A version property a parent POM defines and its modules use is a change of the
parent: when `guava.version` moves in the root `pom.xml`, each module
dependency whose version resolves only through it, such as `${guava.version}`,
is a package of the root `pom.xml`, at the root's value on each side. The
module's own POM lists the same dependency when the module changes. The other
POMs are read from the head checkout: when a change bumps a parent's property
and also edits how a module uses it, the module's dependency resolves at the
head's value on both sides. A project property wins over a profile property of
the same name, since which profile is active is not known.

Left out:

- a dependency without a version, such as one a parent or a BOM manages: the
  change shows on the POM that states the version, when the repository holds
  it;
- a dependency whose name or version keeps a reference that neither the POM
  nor its parent POMs in the repository resolve, such as a property of a
  parent POM published elsewhere;
- a dependency on, or a parent that is, a module of the repository;
- the plugins and their dependencies.

These versions are the ones the POM declares, not the ones Maven resolves: a
version range stays as it is written, and a version a parent or a BOM changes
without a POM of the repository changing does not show. The summary ends each
such change with `declared`, and the pull request note marks its POM
`(declared)`. An SBOM the CI writes, with `cyclonedx-maven-plugin`, lists the
resolved versions and gives the licences and known vulnerabilities of a change
it matches by `group:artifact`.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.gradle_dynamic_settings` | The settings compute an include or a `projectDir`; the projects named with literals stay components. |
| `observe.gradle_project_missing` | An included project has no directory, or lies outside the repository. |
| `observe.gradle_settings_unreadable` | The settings file cannot be read. |
| `observe.maven_module_missing` | A listed module has no POM, or lies outside the repository. |
| `observe.maven_pom_invalid` | A POM does not parse or states no `artifactId`; its modules are not read. |
| `observe.maven_walk_bounded` | The detector stopped after 10,000 POMs. |
