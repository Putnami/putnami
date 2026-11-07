# Observe

`putnami observe` explains a change from what a CI job already produced. It
runs at the end of the job, after your build and your tests, and reads the
repository, the Git history and the reports the job wrote. It needs no Putnami
workspace and changes nothing in your pipeline.

To add it to a pipeline, follow
[Observe a change in your CI](/docs/how-to/observe-a-change-in-your-ci).

`@putnami/observe` is `experimental`: it promises no compatibility yet.

## What it answers

| Question | How it answers |
|---|---|
| Which commit was built, by which CI run? | It reads the CI's environment: the code host, the repository's numeric id, the commit, the run and the pull or merge request. |
| What is the change measured against? | The pull request base, the merge request diff base, or on a branch push the commit before the push. It deepens a shallow clone itself. |
| Which files changed? | The Git diff between the base and the commit. |
| Which parts of the repository changed, and which depend on them? | It reads the repository's build and workspace files to find its components and the edges between them. A component is `changed` when one of its files changed, `dependent` when it depends on a changed component, and `unaffected` otherwise. |
| Which tests ran, and did they pass? | It reads the test reports and places each test case on its component. |
| How well do the tests cover the change? | It reads the coverage reports, places each covered file on its component, and computes the coverage of the changed lines. |
| Which dependencies moved? | It reads each changed lockfile at the base and at the commit, and lists the packages added, removed, upgraded or downgraded. |
| Do those moves fix or bring known vulnerabilities, or change licences? | It reads the software bill of materials (SBOM, the list of a build's packages with their licences and known vulnerabilities) the job wrote, at the base and at the commit. |
| Which specs does the repository keep, and does the change touch them? | It reads the Spec Kit, OpenSpec, Kiro and Gherkin specs, places each on its component, and lists its requirements. |

## What it writes

| Output | Where |
|---|---|
| A text summary | The job log. |
| One JSON document | Standard output, with `--output=json`. |
| The change evidence document | A file, with `--evidence <path>`. It follows the `protocol/evidence` v1 schema; keep it as a CI artifact. |
| A commit status and one summary note | The pull or merge request, with `--publish`. The next run updates the same note. |
| An upload to Putnami Cloud | With `--upload` and a `PUTNAMI_CLOUD_TOKEN`. |

It exits `0` whatever it observes, so a failed test or a missing report never
fails the job. It exits `2` on a usage error, and `1` only when it cannot read
the job context or write its output. A missing or unreadable report, an
unreachable base or a failed post is a diagnostic in the summary.

## What it does not do

- It runs no build tool, package manager, test or SBOM tool. It reads files.
- It queries no vulnerability database: vulnerabilities come from the SBOM
  your job wrote.
- It does not decide whether a change may merge. It records evidence.

## Coverage

Every tool below is read from files. A tool that is not listed is not read.

### CI providers

| CI | What it reads | `--publish` posts with |
|---|---|---|
| GitHub Actions | `GITHUB_*` variables and the event payload | `GITHUB_TOKEN`, with `statuses: write` and `pull-requests: write` |
| GitLab CI, GitLab.com and self-managed | `CI_*` variables | `PUTNAMI_GITLAB_TOKEN`, a project access token with the `api` scope and the Developer role |
| Any other CI | Nothing: name the base with `--base` | Nothing |

Git 2.29 or later is required.

### Repository tools

A detector reads one tool's files and names components. When two detectors
name one directory, an orchestrator's graph wins over a workspace, and a
workspace over a single manifest. When no detector finds a component, the
whole repository is one component.

| Tool | Languages | A component is | Edges come from | Graph export, read when present | Page |
|---|---|---|---|---|---|
| Nx | Any | A project | The graph export, else `project.json`, `nx.json` and `package.json` dependencies | `nx graph --file=graph.json` | [Nx](/docs/tooling-&-workspace/observe/nx) |
| moon 1.x and 2.x | Any | A project | The graph export, else `moon.yml` `dependsOn`, task `deps` and `package.json` dependencies | `moon project-graph --json > moon-graph.json`, or `--moon-graph` | [moon](/docs/tooling-&-workspace/observe/moon) |
| Pants | Python and any language Pants builds | A directory of BUILD targets | The graph export, with the dependencies Pants infers from imports | `pants peek :: > pants-graph.json`, or `--pants-graph` | [Pants](/docs/tooling-&-workspace/observe/pants) |
| Rush | JavaScript, TypeScript | A project of `rush.json` | `package.json` dependencies on another project | None | [Rush](/docs/tooling-&-workspace/observe/rush) |
| npm, pnpm, yarn and bun workspaces, Turborepo, Lerna | JavaScript, TypeScript | A workspace member | `workspace:`, `file:`, `link:` and `portal:` dependencies, and versions a member satisfies | None | [Node workspaces](/docs/tooling-&-workspace/observe/node-workspaces) |
| Composer | PHP | A directory with a `composer.json` | Packages a `path` repository provides | None | [Composer](/docs/tooling-&-workspace/observe/composer-detector) |
| Go modules and `go.work` | Go | A module | `go.mod` requires of another module of the repository, and local `replace` targets | None | [Go](/docs/tooling-&-workspace/observe/go-modules) |
| uv workspaces, Poetry, `pyproject.toml` | Python | A project with a `[project]` or `[tool.poetry]` table | Workspace and path sources | None | [Python](/docs/tooling-&-workspace/observe/python) |
| Gradle, Android included | Java, Kotlin | A project the settings `include` | `project(":x")` and `projects.x` dependencies | None | [Gradle and Maven](/docs/tooling-&-workspace/observe/jvm) |
| Maven 3 and 4 | Java, Kotlin | A module | A dependency on another module | None | [Gradle and Maven](/docs/tooling-&-workspace/observe/jvm) |
| Swift Package Manager | Swift | A package and each of its targets | Target and local package dependencies | `swift package describe --type json > swiftpm.json`, in each package | [iOS and Swift](/docs/tooling-&-workspace/observe/swift) |
| Xcode 15 and later | Swift, Objective-C | The directory of an `.xcodeproj` | The local Swift packages the project references | None | [iOS and Swift](/docs/tooling-&-workspace/observe/swift) |
| Dart, Flutter, pub workspaces, melos | Dart | A package | `path:` dependencies, and inside a workspace a dependency on another of its packages | None | [Dart and Flutter](/docs/tooling-&-workspace/observe/dart) |
| .NET SDK projects and solutions | C#, F#, Visual Basic | A `.csproj`, `.fsproj` or `.vbproj` | `ProjectReference` | None | [.NET](/docs/tooling-&-workspace/observe/dotnet) |
| Bundler, packwerk, gems | Ruby, Rails | A pack, a gem or an application | A pack's `dependencies`, `Gemfile` `path:` gems, gemspec dependencies on a local gem | None | [Ruby and Rails](/docs/tooling-&-workspace/observe/ruby) |
| Cargo | Rust | A crate | Path dependencies, and workspace dependencies with a path | None | [Rust](/docs/tooling-&-workspace/observe/rust) |
| Bazel, Bzlmod or `WORKSPACE` | Any | A package with a `BUILD` file | The query export | `bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json`, or `--bazel-graph` | [Bazel](/docs/tooling-&-workspace/observe/bazel) |

Without its graph export, Pants and Bazel components have no edges, and the
summary names the command that writes the export.

### Test reports

| Format | Written by |
|---|---|
| JUnit XML | Jest with jest-junit, Vitest, `node:test`, PHPUnit `--log-junit`, gotestsum, go-junit-report, pytest `--junitxml`, `pants test --test-report`, `bazel test`, Gradle, Android, Maven Surefire and Failsafe, cargo-nextest, xcbeautify `--report junit`, fastlane `scan`, `swift test --xunit-output`, junitreport's `tojunit` for Dart and Flutter, rspec_junit_formatter, minitest-reporters, `dotnet test --logger junit` |
| TRX | `dotnet test --logger trx`, with xUnit, NUnit or MSTest |

### Coverage reports

| Format | Written by |
|---|---|
| LCOV | Jest, Vitest, c8, bun, `flutter test --coverage`, cargo-llvm-cov `--lcov`, `llvm-cov export -format=lcov`, `bazel coverage --combined_report=lcov`, simplecov-lcov |
| Cobertura XML | pytest-cov `--cov-report=xml`, Pants `--coverage-py-report=xml`, Coverlet, cargo-llvm-cov `--cobertura`, xcresultparser, slather, simplecov-cobertura, Jest, Vitest, c8, PHPUnit `--coverage-cobertura` |
| Clover XML | PHPUnit `--coverage-clover`, Jest, Vitest |
| JaCoCo XML | The JaCoCo Maven plugin, the Gradle `jacoco` plugin, Kover, Android unit test coverage |
| Go cover profile | `go test -coverprofile` |

`putnami observe` finds the reports by their content, anywhere in the
checkout except `.git`, `node_modules`, `vendor` and build outputs that hold
no report. `--junit` and `--coverage` name them instead. It reads no
`.xcresult` bundle: convert it to JUnit and Cobertura first. See
[Reports](/docs/tooling-&-workspace/observe/reports) for what each format
gives.

### Lockfiles

| Lockfile | Ecosystem |
|---|---|
| `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock` | `npm` |
| Rush's `common/config/rush/pnpm-lock.yaml`, `npm-shrinkwrap.json` and `yarn.lock`, and subspace lockfiles | `npm` |
| `composer.lock` | `composer` |
| `go.mod` | `go` |
| `uv.lock`, `poetry.lock`, and the Pants lockfiles `[python.resolves]` names | `pypi` |
| `gradle.lockfile`, `settings-gradle.lockfile`, `gradle/libs.versions.toml` | `maven` |
| `Package.resolved` | `swift` |
| `Podfile.lock` | `cocoapods` |
| `pubspec.lock` | `pub` |
| `packages.lock.json`, else `Directory.Packages.props` | `nuget` |
| `Gemfile.lock` | `gem` |
| `Cargo.lock` | `cargo` |

Maven has no lockfile, and `bun.lock` is not read, so their dependency
changes do not show. A dependency ecosystem can differ from the component's:
a Gradle project is a `gradle` component whose dependencies are `maven`
packages.

### Software bills of materials

| Format | Written by |
|---|---|
| CycloneDX 1.4, 1.5 and 1.6, JSON | osv-scanner, Trivy, Syft, `cyclonedx-maven-plugin` |
| SPDX 2.3, JSON | Syft, osv-scanner. Most SPDX writers record no vulnerability: use CycloneDX for them. |

`--sbom` and `--base-sbom` name the documents of the commit and of the base.
Without `--sbom`, `putnami observe` reads the SBOM it finds in the checkout.
When it finds several, it reads none and says so.

## Reference

| Page | What it covers |
|---|---|
| [What `putnami observe` reads](/docs/tooling-&-workspace/observe/observe) | Every flag, how each CI and the base are read, report discovery, the evidence document, upload and publishing |
| [How `putnami observe` explains a change](/docs/tooling-&-workspace/observe/change-analyses) | Components, tests and coverage, changed-line coverage, dependency changes, licences and vulnerabilities |
| [Reports](/docs/tooling-&-workspace/observe/reports) | What each test, coverage and SBOM format gives, and why a report is rejected |
| One page per repository tool | The pages in the repository tools table |
