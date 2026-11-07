# Observe a change in your CI

You will add one step to a GitHub Actions workflow, or one job to a GitLab CI pipeline, that explains each change from what your tests already produced. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

For each pull or merge request, the step answers:

- which commit was built, and which commit the change is measured against;
- which files changed, which parts of the repository they belong to, and which parts depend on them;
- which tests passed or failed, and how much of the changed lines they covered;
- which dependency versions the lockfiles moved and, with a software bill of materials (SBOM), the licences and known vulnerabilities each move brings or removes.

It writes the answers in the job log, as a change evidence document you keep as an artifact, and as a status and a note on the pull or merge request. It never fails your job.

`putnami observe` comes with the `@putnami/observe` extension. It is `experimental`: being on the stable channel is not a compatibility promise. [Observe](/docs/tooling-&-workspace/observe) lists every CI, repository tool, report format, lockfile and SBOM format it reads.

## What the step installs

On GitHub Actions, the step is the action `Putnami/observe-action@v1`, from the GitHub Marketplace. On GitLab CI, the job runs the same lines itself:

| Line | What it does |
|---|---|
| `curl -fsSL https://putnami.dev/install.sh \| bash` | Installs the stable Putnami CLI into `~/.putnami/bin`. |
| `export PATH="$HOME/.putnami/bin:$PATH"` | Makes `putnami` reachable in the rest of the step. A script piped into `bash` cannot change the `PATH` of the shell that runs it. |
| `putnami extensions install --user @putnami/observe` | Installs the extension in the user scope, so its command runs outside any workspace. `--user` takes one extension per command. |
| `putnami extensions install --user @putnami/cloud` | Installs the extension that uploads the evidence to Putnami Cloud. The action runs this line only when its `token` input is set. |
| `putnami observe --evidence observe-evidence.json --publish` | Prints the summary, writes the evidence document, and posts a commit status and a summary note on the pull or merge request. It exits `0` whatever it observes: only a usage error, or an evidence file it cannot write, fails it. |

`putnami observe` finds the base of the change from the CI: the pull request base on GitHub, the merge request diff base on GitLab, and on a branch push the commit before the push. It deepens a shallow clone itself, so you do not change `fetch-depth` or `GIT_DEPTH`. It finds JUnit, TRX, LCOV, Cobertura, Clover, JaCoCo and Go cover reports by their content, anywhere in the checkout except `.git`, `node_modules`, `vendor` and the Gradle and Maven build outputs that hold no report, and the reports of Bazel through its `bazel-testlogs` and `bazel-out` links.

The install needs the network: when the installer or the registry cannot be reached, the GitHub step or the GitLab job fails.

## Steps

### 1) Write the reports your stack needs

`putnami observe` reads the test and coverage reports your job writes; it runs no test itself. Find your stack below and add what it says to your test step. A repository that mixes stacks, such as a Go API and a Node front end, follows each section that applies.

#### Node: npm, pnpm, yarn, bun, Turborepo, Lerna

With Vitest, write a JUnit report and coverage:

```bash
npx vitest run --coverage --reporter=default --reporter=junit --outputFile.junit=junit.xml
```

With Jest, add `jest-junit` to the dev dependencies, then run:

```bash
npx jest --ci --coverage --reporters=default --reporters=jest-junit
```

Each member of the workspace that `package.json` `workspaces` or `pnpm-workspace.yaml` lists is a component. A `workspace:`, `file:`, `link:` or `portal:` dependency on another member is an edge, and so is a version range another member satisfies. A `package-lock.json`, `pnpm-lock.yaml` or `yarn.lock` change is a dependency change of ecosystem `npm`. A `bun.lock` change is not read.

#### Nx

Run your tests as Node's section says, then write the project graph before the observe step:

```bash
npx nx graph --file=graph.json
```

`putnami observe` reads `graph.json` at the repository root. It holds the edges Nx computes from source imports, so a change to a library marks every project that imports it. Without it, the projects and edges come from `project.json`, `nx.json` and the `package.json` dependencies.

#### Rush

Run your tests as Node's section says. Each project of `rush.json` is a component, and a `package.json` dependency on another Rush project is an edge. A change to the lockfile under `common/config/rush`, or to a subspace lockfile, is a dependency change of ecosystem `npm`.

#### moon

In a moon monorepo, `putnami observe` places each change on the moon project that holds it, whatever its language, and marks the projects whose `moon.yml` `dependsOn` or task `deps`, or whose `package.json` dependencies, name it. moon also infers edges from its language toolchains, which only its own graph holds: to use them, write the graph before the observe line, in a step where `moon` is installed:

```bash
moon project-graph --json > moon-graph.json
```

`putnami observe` reads `moon-graph.json` at the repository root; `--moon-graph <path>` names another file. Without it, `putnami observe` reads `.moon/workspace.yml` and each `moon.yml` itself.

#### Go

Write a JUnit report with [gotestsum](https://github.com/gotestyourself/gotestsum) and a cover profile:

```bash
go run gotest.tools/gotestsum@latest --junitfile reports/junit.xml -- -coverprofile=reports/coverage.out ./...
```

Each module is a component, a member of `go.work` or any directory with a `go.mod`, and a `require` of another module of the repository is an edge. Each test case and each covered file lands on its module by import path. A `go.mod` change to a required version is a dependency change of ecosystem `go`.

#### PHP and Composer

Have PHPUnit write a JUnit report and Clover coverage, with Xdebug or PCOV enabled:

```bash
vendor/bin/phpunit --log-junit junit.xml --coverage-clover clover.xml
```

Each directory with a `composer.json` is a component, and a package a `path` repository provides is an edge. A `composer.lock` change is a dependency change of ecosystem `composer`.

#### Python

In a Python repository, have pytest write both reports, with pytest-cov installed:

```bash
pytest --junitxml=junit.xml --cov --cov-report=xml
```

`putnami observe` places each test case and each covered file on the project that holds it: a member of the uv workspace, or a directory whose `pyproject.toml` has a `[project]` or `[tool.poetry]` table. In a uv workspace, run it from the root as `uv run --all-packages pytest …`, so every member is installed. pytest names each test case from its file's path relative to its `rootdir`: if pytest takes another `rootdir`, for example a project's directory that holds the pytest configuration, add `--rootdir=.` so the names stay relative to the repository root. A `uv.lock` or `poetry.lock` change is a dependency change of ecosystem `pypi`.

#### Pants

In a Pants repository, write the export of the target graph and run the tests with their reports, before the observe step:

```bash
pants peek :: > pants-graph.json
pants test --test-report --use-coverage --coverage-py-report=xml ::
```

`pants peek` lists every target with the dependencies Pants infers from imports, so `putnami observe` knows that a change to a library marks the code that imports it. Each directory of BUILD targets is a component; without the export, the components have no edge, and the summary says how to write it. The JUnit reports under `dist/test/reports` and the coverage under `dist/coverage` land on the components, and a change to a lockfile that `[python.resolves]` in `pants.toml` names is a dependency change of ecosystem `pypi`. Write the export elsewhere with `--pants-graph <path>`.

#### Bazel

In a Bazel repository, write the query export before `putnami observe`, in the job that ran `bazel test` or `bazel coverage --combined_report=lcov`:

```bash
bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json
```

`putnami observe` makes each Bazel package a component, reads the edges between packages from `bazel-graph.json`, and finds the logs of `bazel-testlogs` and the combined LCOV report of `bazel-out/_coverage/_coverage_report.dat` through the links Bazel leaves at the root. Each test log goes to the package of its test target. Without the export, the packages come from the `BUILD` files, without edges, and the summary names the command. The links point into Bazel's output base, so on GitLab run these lines in the job that ran Bazel, not in a later one.

#### Gradle, Android included

In a Gradle repository, Android included, apply the `jacoco` plugin with its XML report in each project, and run the tests before the observe step:

```kotlin
plugins {
    jacoco
}

tasks.jacocoTestReport {
    reports {
        xml.required = true
    }
}
```

```bash
./gradlew test jacocoTestReport
```

Each project the settings `include` is a component, and its `project(":x")` and `projects.x` dependencies are its edges. The JUnit reports under `build/test-results` and the JaCoCo or Kover reports under `build/reports` land on the projects. A change to a `gradle.lockfile`, which `./gradlew dependencies --write-locks` writes when dependency locking is on, or to `gradle/libs.versions.toml` is a dependency change of ecosystem `maven`. On Android, run `./gradlew testDebugUnitTest createDebugUnitTestCoverageReport` with `enableUnitTestCoverage = true` on the debug build type.

#### Maven

In a Maven repository, add the `prepare-agent` and `report` goals of `jacoco-maven-plugin` to the build, then run `mvn verify`:

```xml
<plugin>
  <groupId>org.jacoco</groupId>
  <artifactId>jacoco-maven-plugin</artifactId>
  <version>0.8.13</version>
  <executions>
    <execution><goals><goal>prepare-agent</goal></goals></execution>
    <execution><id>report</id><phase>verify</phase><goals><goal>report</goal></goals></execution>
  </executions>
</plugin>
```

Each module the POMs list is a component, and a dependency on another module is an edge. The reports under `target/surefire-reports`, `target/failsafe-reports` and `target/site/jacoco` land on the modules. Maven has no lockfile, so a Maven repository has no dependency change. To read only some reports, name them, for example `--junit '**/target/surefire-reports/*.xml' --coverage '**/target/site/jacoco/jacoco.xml'`.

#### iOS and Swift

In an iOS or Swift repository, write each package's description and convert the `.xcresult` bundle to JUnit and Cobertura, before the observe step. The same lines run in a GitHub Actions job on `runs-on: macos-15` and in a GitLab job on a macOS runner (`tags: [saas-macos-medium-m1]`), with xcbeautify and xcresultparser installed (`brew install xcbeautify xcresultparser`):

```bash
(cd Packages/Pricing && swift package describe --type json > swiftpm.json)
set -o pipefail
xcodebuild test -workspace Shop.xcworkspace -scheme Shop -destination 'platform=iOS Simulator,name=iPhone 16' \
  -enableCodeCoverage YES -resultBundlePath build/Test.xcresult | xcbeautify --report junit --report-path build/reports
xcresultparser --output-format cobertura --project-root . build/Test.xcresult > build/reports/coverage.xml
```

For a package on its own, `swift test --enable-code-coverage --xunit-output build/reports/xunit.xml` writes the JUnit report, and `llvm-cov export -format=lcov` the coverage. `putnami observe` reads no `.xcresult` itself. Each Swift package, each of its targets and each Xcode project is a component; the export gives the targets' dependencies, and a local package the Xcode project references is an edge, so a change to a package marks the packages and the app that use it. Without the export, the summary says how to write it. A test case named `PricingTests.PriceCalculatorTests` lands on the test target `PricingTests`, and a change to `Package.resolved` or `Podfile.lock` is a dependency change of ecosystem `swift` or `cocoapods`.

#### Dart and Flutter

In a Dart or Flutter repository, run each package's tests with coverage and a JUnit report, with [junitreport](https://pub.dev/packages/junitreport) activated by `dart pub global activate junitreport`:

```bash
melos exec --dir-exists=test -- "flutter test --machine --coverage | tojunit --output test-results.xml"
```

Without melos, run the same pipe in each package's directory, or `dart test --reporter json | tojunit --output test-results.xml` for a plain Dart package. Each package with a `pubspec.yaml` is a component: a member of the pub workspace or of the `melos.yaml` packages, or any other package, and a `path:` dependency, or inside a workspace a dependency on another of its packages, is an edge. Each package's `coverage/lcov.info` reads from that package's directory, each test case lands on the package of its test file, and a `pubspec.lock` change is a dependency change of ecosystem `pub`.

#### .NET

In a .NET repository, have `dotnet test` write a TRX report and Coverlet's coverage, with the `coverlet.collector` package in each test project:

```bash
dotnet test --logger trx --results-directory TestResults --collect:"XPlat Code Coverage"
```

Each project file is a component, a `workspace` one when a `.sln` or `.slnx` at the root lists it, and each `ProjectReference` is an edge, so a change to a library marks the projects and test projects that reference it. Each test case lands on the project of its test assembly, and the coverage under `TestResults/` on the projects that hold the covered files. A `packages.lock.json` change is a dependency change of ecosystem `nuget`; without lock files, so is a version change in `Directory.Packages.props`.

#### Ruby and Rails

In a Ruby or Rails repository, have RSpec write a JUnit report with `rspec_junit_formatter`, and SimpleCov a Cobertura report with `simplecov-cobertura`, both in the `Gemfile`'s test group. Start SimpleCov at the top of `spec/spec_helper.rb`, before the application loads:

```ruby
require "simplecov"
require "simplecov-cobertura"
SimpleCov.start do
  formatter SimpleCov::Formatter::CoberturaFormatter
end
```

```bash
bundle exec rspec --format progress --format RspecJunitFormatter --out rspec.xml
```

`putnami observe` places each test case and each covered file on the component that holds it: a packwerk pack, a directory with a `package.yml`; a gem; or the application, a directory with a `Gemfile`. The `dependencies` of a pack's `package.yml`, a `Gemfile` gem with a `path`, and a gemspec dependency on a local gem are edges, so a change to a pack marks the packs that depend on it. With Minitest, `minitest-reporters`' `JUnitReporter` and `simplecov-lcov` write reports it reads too. A `Gemfile.lock` change is a dependency change of ecosystem `gem`.

#### Rust

In a Rust repository, give cargo-nextest a profile that writes a JUnit report, in `.config/nextest.toml`:

```toml
[profile.ci.junit]
path = "junit.xml"
```

Then run the tests through cargo-llvm-cov, with both installed (`cargo install cargo-nextest cargo-llvm-cov`):

```bash
cargo llvm-cov nextest --profile ci --workspace --lcov --output-path lcov.info
```

nextest writes `target/nextest/ci/junit.xml`, and `putnami observe` reads it there, without walking the rest of the target directory. Each case lands on the crate its test binary names, and each covered file on the crate that holds it: a member of the root `[workspace]`, or a directory whose `Cargo.toml` has a `[package]` table. A path dependency, or a `workspace = true` one whose `[workspace.dependencies]` entry has a path, is an edge. A `Cargo.lock` change is a dependency change of ecosystem `cargo`. When `CARGO_TARGET_DIR` puts the target directory outside the checkout, name the report with `--junit`.

### 2) GitHub Actions: add a step to your test job

Add this step to the job that runs your tests, after the step that writes the reports:

```yaml
      - uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
```

`if: ${{ !cancelled() }}` runs the step when a test fails too: a failed test is part of the evidence. The job needs a Linux or macOS runner, such as `ubuntu-latest`. Keep your `actions/checkout` step as it is: the default `fetch-depth: 1` works.

Without a token, the step prints the summary in the job log and writes the evidence document. To also post the commit status and the summary note on the pull request, give the job these permissions and set `publish`:

```yaml
    permissions:
      contents: read
      statuses: write
      pull-requests: write
    steps:
      # ... your checkout, build and test steps ...
      - id: observe
        uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
        with:
          publish: true
      - uses: actions/upload-artifact@v4
        if: ${{ !cancelled() }}
        with:
          name: observe-evidence
          path: ${{ steps.observe.outputs.evidence }}
```

The action passes the run's `GITHUB_TOKEN` to `putnami observe` only when `publish` is on. The `evidence` output is the path of the evidence document; `actions/upload-artifact` keeps it. The action's inputs `junit`, `coverage`, `sbom`, `base-sbom` and `base` pass the flags of the same name, and `channel` picks the release channel of the CLI and the extensions, `stable` by default. A token set in the step's `env` is not read: pass it as the `token` input. Its [README](https://github.com/Putnami/observe-action#readme) lists them.

### 3) GitLab CI: add a job after your test job

Add this job to `.gitlab-ci.yml`. It reads the reports from the artifacts of the job named `test`, so that job must keep its reports as artifacts with `when: always`:

```yaml
observe:
  stage: test
  image: buildpack-deps:bookworm-scm
  needs:
    - job: test
      artifacts: true
  when: always
  before_script: []
  cache: []
  script:
    - curl -fsSL https://putnami.dev/install.sh | bash
    - export PATH="$HOME/.putnami/bin:$PATH"
    - putnami extensions install --user @putnami/observe
    - putnami extensions install --user @putnami/cloud
    - putnami observe --evidence observe-evidence.json --publish
    # With PUTNAMI_CLOUD_TOKEN set as a masked CI/CD variable, this line uploads the evidence to Putnami Cloud:
    # - putnami observe --evidence observe-evidence.json --publish --upload
  artifacts:
    when: always
    paths:
      - observe-evidence.json
```

`--publish` needs a project access token, because the GitLab job token cannot write notes. In the project, open **Settings > Access tokens**, add a token with the **Developer** role and the `api` scope, then store it as the masked CI/CD variable `PUTNAMI_GITLAB_TOKEN`. A self-managed GitLab works the same way: the job reads its API URL from `CI_API_V4_URL`.

Rename `test` in `needs` if your test job has another name. The image holds Bash, curl, tar, `sha256sum` and Git; any image with those, and Git 2.29 or later, works. `before_script: []` and `cache: []` keep your pipeline's defaults out of this job.

### 4) Read the job log

The step prints a summary like this one, from a Go repository:

```text
putnami observe: GitHub Actions, github.com/Putnami/demo-observe-go (project 1442100792)
  commit   bd5c750895a5, on pricing-discount, pull request #1
  base     eb60e16d8997, merge base with main from the pull request; the shallow clone was deepened by up to 64 commits
  changed  7 files
    modified  .github/workflows/ci.yml
    modified  api/go.mod
    ...
  reports  2 reports
    go-cover  reports/coverage.out
    junit     reports/junit.xml
```

A report it cannot find or read is a warning in the same summary, such as `observe.report_missing`. The evidence document is the `observe-evidence` artifact of the job.

### 5) Read the pull or merge request

On a pull or merge request, `--publish` sets the commit status `putnami/observe` on the pull request's head commit, or on the commit of the merge request pipeline, with a one-line summary such as `1 changed, 2 dependent components; changed lines 80.0% covered; 1 dependency change`. It also writes one summary note with the components, their tests and coverage, the least covered changed lines and the dependency changes. The next run updates the same note instead of adding one.

Without the token, or on a branch push, the summary says why nothing was posted, and the job still passes. On a pull request from a fork, GitHub gives a read-only `GITHUB_TOKEN` and refuses the post; the job still passes.

### 6) Upload the evidence to Putnami Cloud

To send the evidence to Putnami Cloud, store a workspace token with the `delivery.ingest` scope as the secret or masked CI/CD variable `PUTNAMI_CLOUD_TOKEN`. The token names the workspace.

On GitHub, pass it to the action as `token: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}`. With a token, `publish` is on unless you set it to `false`, so the job needs the permissions of step 2. On GitLab, use the commented `--upload` line in place of the line above it. An upload that fails is a diagnostic in the summary, and the job still passes.

### 7) Add licences and known vulnerabilities from an SBOM

A software bill of materials (SBOM) lists the packages of a build with their licences and known vulnerabilities. `putnami observe` reads one the job wrote, CycloneDX 1.4 to 1.6 or SPDX 2.3 in JSON, and gives each changed dependency what it lists: the vulnerabilities the change fixes or introduces, and the licences before and after. It queries no vulnerability database itself.

On GitHub Actions, put this step before the observe step. It writes an SBOM of the head and one of the base with [osv-scanner](https://github.com/google/osv-scanner), which reads the lockfiles of Node, Composer, Go, Python, Dart, Ruby and Rust projects, `uv.lock`, `poetry.lock`, `pubspec.lock`, `Gemfile.lock` and `Cargo.lock` included. The base is the pull request base, or on a push the commit before the push:

```yaml
      - name: Write the SBOMs
        if: ${{ !cancelled() }}
        env:
          BASE_SHA: ${{ github.event.pull_request.base.sha || github.event.before }}
        run: |
          curl -fsSL -o "$RUNNER_TEMP/osv-scanner" https://github.com/google/osv-scanner/releases/download/v2.7.0/osv-scanner_linux_amd64
          chmod +x "$RUNNER_TEMP/osv-scanner"
          sbom() {
            "$RUNNER_TEMP/osv-scanner" scan source --all-packages --licenses --format cyclonedx-1-5 \
              --output-file "$RUNNER_TEMP/$1.cdx.json" -r "$2" || true
          }
          sbom head .
          if [ -n "$BASE_SHA" ] && git fetch --quiet --depth=1 origin "$BASE_SHA"; then
            git worktree add --quiet --detach "$RUNNER_TEMP/base" "$BASE_SHA"
            sbom base "$RUNNER_TEMP/base"
          fi
```

Then name both documents in the action's inputs:

```yaml
      - uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
        with:
          sbom: ${{ runner.temp }}/head.cdx.json
          base-sbom: ${{ runner.temp }}/base.cdx.json
```

On GitLab, name them on the observe line:

```bash
putnami observe --evidence observe-evidence.json --sbom /tmp/head.cdx.json --base-sbom /tmp/base.cdx.json
```

osv-scanner exits `1` when it finds a vulnerability, so `|| true` keeps the step green. A document it could not write, such as the base one on the first push of a branch, is a warning in the summary, and that side is unknown. `--all-packages` lists every package, not only the vulnerable ones, and `--licenses` adds their licences. osv-scanner writes no vulnerability in SPDX, so use CycloneDX. It names a vulnerable package without its version: when the checkout locks two versions of one package, in two lockfiles, both get its vulnerabilities. On GitLab, use `$CI_MERGE_REQUEST_DIFF_BASE_SHA` as the base, and a directory outside the checkout, such as `/tmp`, in place of `$RUNNER_TEMP`. [Trivy](https://trivy.dev) also writes a CycloneDX document with vulnerabilities: `trivy fs --format cyclonedx --scanners vuln --output head.cdx.json .`.

Without `--base-sbom`, each dependency gets its state at the head and the summary says the base is unknown. Without `--sbom`, `putnami observe` reads an SBOM it finds in the checkout, in a `.json` file whose name contains `bom`, `cdx`, `cyclonedx` or `spdx`. The summary then gives each change one more line:

```text
  dependencies  3 changes, SBOM head CycloneDX 1.5, base CycloneDX 1.5
    upgraded   lodash 4.17.20 -> 4.17.21, in package-lock.json
               fixes 2 (GHSA-35jh-r3h4-6jhm high, GHSA-29mw-wpgm-hmr9 medium), introduces 0, keeps 3; license MIT
    added      zod 3.23.8, in package-lock.json
               introduces 1 (GHSA-mrhx-c7xc-m3w5 high); license MIT
```

## Give your agents the same view

The CI job reads what a change touches; your coding agents can read the same thing on your machine, in a repository that still has no `putnami.workspace.json`. Install two extensions in your user scope:

```bash
putnami extensions install --user @putnami/observe
putnami extensions install --user @putnami/contributor
```

The second command writes five skills to `~/.claude/skills` for Claude Code and `~/.agents/skills` for Codex, and writes nothing in your repository. A file you already keep at one of those paths is never overwritten: the install stops, names it, and writes nothing. `putnami extensions remove --user @putnami/contributor` removes them again.

| Skill | What it does without a workspace |
|---|---|
| `context` | Answers "what does this change touch?" from `putnami observe --output=json`: the changed components, the ones that depend on them, their tests and coverage when reports exist, and the specs they keep. |
| `spec` | Adds or changes a requirement in the Spec Kit, OpenSpec, Kiro or Gherkin spec your repository keeps, in that format, then runs `putnami observe` to read it back. |
| `build`, `check`, `design` | Say in one line that they need a Putnami workspace, and stop. |

Ask your agent "what does this change touch?" or "add a requirement to the export spec". `putnami observe` lists the specs it reads in its summary and in the `specs` member of its JSON document, so a requirement the agent adds shows up on the next CI run too.

Inside a Putnami workspace, each of these skills follows the copy the workspace pins instead.

## Learn more

- [Observe](/docs/tooling-&-workspace/observe): what `putnami observe` answers, and every CI, repository tool, report format, lockfile and SBOM format it reads.
- [What `putnami observe` reads](/docs/tooling-&-workspace/observe/observe): every flag, how the base is found, and how reports and SBOMs are discovered.
- [How `putnami observe` explains a change](/docs/tooling-&-workspace/observe/change-analyses): components, tests, changed-line coverage and dependency changes.
- [Reports](/docs/tooling-&-workspace/observe/reports): what each test, coverage and SBOM format gives.

You now have a CI job that records which files a change touched and which tests and coverage reports it produced, as an artifact you can keep or upload to Putnami Cloud.
