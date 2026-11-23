# Observe a change in your CI

You will run `putnami observe` once on your laptop, open the full report of your change, then add one step to a GitHub Actions workflow, or one job to a GitLab CI, Jenkins, CircleCI, Buildkite, Azure Pipelines or Bitbucket Pipelines pipeline, that explains each change from what your tests already produced. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

For each pull or merge request, the step answers:

- which commit was built, and which commit the change is measured against;
- which files changed, which parts of the repository they belong to, and which parts depend on them;
- which tests passed or failed, and how much of the changed lines they covered;
- which dependency versions the lockfiles moved and, with a software bill of materials (SBOM), the licences and known vulnerabilities each move brings or removes;
- which MADR decision records govern the files it touches, and which records it adds, edits or deletes.

It writes a short summary in the log, which ends with the one command that keeps the full report, and the whole answer as a change evidence document you keep as an artifact. Once it uploads the evidence, it also posts a status and a note on the pull or merge request that link to the full report when the reporter answers its URL, else to the CI run. It never fails your job.

`putnami observe` comes with the `@putnami/observe` extension. It is `preview` in the [support catalog](/docs/support): the change evidence document it writes keeps version 1 by addition only, so a document that validates today keeps validating, while its flags, its text summary and its note may still change in a minor release whose notes say so. [Observe](/docs/tooling-&-workspace/observe) lists every CI, repository tool, report format, lockfile and SBOM format it reads.

## Start on your laptop

Install the CLI and the two extensions in your user scope, then run `putnami observe` in your checkout, on the branch of your change:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
putnami extensions install --user @putnami/observe
putnami extensions install --user @putnami/cloud
putnami observe
```

Outside a CI, `putnami observe` measures the change against the default branch of your `origin` remote, so it needs no flag. It prints a short summary and sends nothing off your machine. Its last line names the next command:

```text
Next: run `putnami observe --open` to sign in and open the full report
```

`putnami observe --open` runs the same analysis, keys the repository by the numeric id GitHub or GitLab gives it, signs you in to Putnami Cloud in your browser when you are not signed in yet, uploads the evidence and opens the full report. The next runs reuse the session. For a private repository, set `GITHUB_TOKEN` or `PUTNAMI_GITLAB_TOKEN` to a token that reads it; without one, the summary says which to set. `putnami observe --details` prints the whole analysis in the log instead of the short summary.

### A repository whose CI kept nothing

A CI deletes its logs and artifacts after a retention period, so an old repository set aside has no report left to read. Run `putnami observe --history --open` in its checkout instead: it analyzes the last 14 days of the default branch on your laptop, or the last 14 days of activity when the branch has been idle since, one commit at a time, and uploads them. It samples at most 100 commits, newest first, reads no test or coverage report, and stops after 10 minutes. Running it again keeps one run per commit in Putnami Cloud. `putnami observe --history 30d` observes a longer window; [The recent history](/docs/tooling-&-workspace/observe/observe#the-recent-history) gives the details.

## What the step installs

On GitHub Actions, the step is the action `Putnami/observe-action@v1`, from the GitHub Marketplace. On GitLab CI, the job runs the same lines itself:

| Line | What it does |
|---|---|
| `curl -fsSL https://putnami.dev/install.sh \| bash` | Installs the stable Putnami CLI into `~/.putnami/bin`. |
| `export PATH="$HOME/.putnami/bin:$PATH"` | Makes `putnami` reachable in the rest of the step. A script piped into `bash` cannot change the `PATH` of the shell that runs it. |
| `putnami extensions install --user @putnami/observe` | Installs the extension in the user scope, so its command runs outside any workspace. `--user` takes one extension per command. |
| `putnami extensions install --user @putnami/cloud` | Installs the extension that uploads the evidence to Putnami Cloud. The action runs this line only when its `token` input is set. |
| `putnami observe --evidence observe-evidence.json` | Prints the summary and writes the evidence document. It exits `0` whatever it observes: only a usage error fails it. An evidence file it cannot write is a warning in the summary. |
| `putnami observe --evidence observe-evidence.json --upload --publish` | Also uploads the evidence, then posts a commit status and a summary note on the pull or merge request that link to the report, or to the CI run when the reporter answers no report URL. `--publish` needs `--upload`. The action runs this line in place of the one above when its `token` input is set. |

`putnami observe` finds the base of the change from the CI: the pull request base on GitHub, the merge request diff base on GitLab, and on a branch push the commit before the push. It deepens a shallow clone itself, so you do not change `fetch-depth` or `GIT_DEPTH`. It finds JUnit, TRX, LCOV, Cobertura, Clover, JaCoCo and Go cover reports by their content, anywhere in the checkout except `.git`, `node_modules`, `vendor` and the Gradle and Maven build outputs that hold no report, and the reports of Bazel through its `bazel-testlogs` and `bazel-out` links.

The install needs the network: when the installer or the registry cannot be reached, the GitHub step or the GitLab job fails.

## What leaves the CI

`putnami observe` reads the checkout and the reports on the machine that runs it. Without `--upload` or `--open`, nothing about the change leaves that machine: it prints the summary and writes the evidence document where `--evidence` names it, and your CI keeps that file as an artifact. Its only other network use is Git, which fetches from your `origin` remote the history the base of the change needs, deepening a shallow clone.

`--open` uploads too. With either flag, `putnami observe` first asks the GitHub or GitLab API for the repository's numeric id when it runs outside a CI or in a CI other than GitHub Actions and GitLab CI, with `GITHUB_TOKEN` or `PUTNAMI_GITLAB_TOKEN` when one is set; GitHub Actions and GitLab CI state that id themselves. It then hands the evidence document to the `@putnami/cloud` extension, which sends Putnami Cloud these fields, with `PUTNAMI_CLOUD_TOKEN`, the short-lived token a GitHub Actions or gitlab.com job gets for its own ID token, or your signed-in session as the credential. That GitHub Actions job first sends its ID token, signed by GitHub and naming the repository, its owner, the workflow, the ref and the actor that started the run, with your organization's id to `auth.putnami.cloud`; that GitLab job sends its ID token, signed by gitlab.com and naming the project, its group, the pipeline, the job, the ref, and the id, login and email address of the user that started the pipeline, with your organization's id to `auth.putnami.cloud`. The evidence then carries:

| Part | Fields |
|---|---|
| The run | The code host and its host name; the repository's path, such as `owner/name`, and its numeric id; the commit SHA and the branch; the pull or merge request number; where it ran: GitHub Actions, GitLab CI, another CI or a laptop; an upload id built from the CI run, attempt and job ids, or a random one on a laptop; whether a test failed; the Git tree id the job tested and the merge base of the change |
| Tests | For each component: the passed, failed and skipped counts, and for each failing test case its name, its test file and its failure message |
| Coverage | For each component: the covered and the total lines |
| Components | For each component: its directory, its name, its ecosystem, how it was found, whether the change touches it or depends on it, and the components it depends on |
| Changed-line coverage | The covered and total changed lines, and for each changed file the coverage instruments: its path, its counts and its uncovered line numbers |
| Dependency changes | For each package a lockfile moved: its ecosystem, its lockfile, its name, its versions before and after, and its component; with an SBOM, its licences and its known vulnerabilities, by advisory id and severity |
| Specs and declarations | Only in a repository that holds a Putnami workspace: the spec verification record its CI wrote, and each declaration file the change touches, such as a spec, an architecture manifest, a decision record, a CI configuration or a `CODEOWNERS` file, with its content before and after the change |

Your source code apart from those declaration files, the full list of changed files, which decision records govern the change, the architecture rules and the diagnostics of the summary stay in the evidence document, on your CI. The CLI is public: [`tooling/observe`](https://github.com/Putnami/putnami/tree/main/tooling/observe) writes the document, and [`cloud/extension`](https://github.com/Putnami/putnami/tree/main/cloud/extension) sends it, so you can check this list against the code. `PUTNAMI_EVIDENCE_REPORTER` names another extension to hand the document to instead.

`--publish` writes to your code host, not to Putnami Cloud: with the code host token, it posts the commit status and the summary note that [step 5](#5-read-the-pull-or-merge-request) describes. The CLI's own usage telemetry is separate, and [CLI telemetry](/docs/concepts/cli-telemetry) lists its fields and how to turn it off.

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

Each member of the workspace that `package.json` `workspaces` or `pnpm-workspace.yaml` lists is a component. A `workspace:`, `file:`, `link:` or `portal:` dependency on another member is an edge, and so is a version range another member satisfies. A `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock` or `bun.lock` change is a dependency change of ecosystem `npm`. Bun's binary `bun.lockb` is not read, and a change to it says so in a diagnostic.

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

Each module the POMs list is a component, and a dependency on another module is an edge. The reports under `target/surefire-reports`, `target/failsafe-reports` and `target/site/jacoco` land on the modules. Maven has no lockfile: a change to a version a POM declares, in `dependencies`, `dependencyManagement`, a property or the parent, is a dependency change of ecosystem `maven`, marked declared, since Maven may resolve another version. To read only some reports, name them, for example `--junit '**/target/surefire-reports/*.xml' --coverage '**/target/site/jacoco/jacoco.xml'`.

#### iOS and Swift

In an iOS or Swift repository, write each package's description and keep the `.xcresult` bundle, before the observe step, in the same macOS job. The same lines run in a GitHub Actions job on `runs-on: macos-15` and in a GitLab job on a macOS runner (`tags: [saas-macos-medium-m1]`):

```bash
(cd Packages/Pricing && swift package describe --type json > swiftpm.json)
xcodebuild test -workspace Shop.xcworkspace -scheme Shop -destination 'platform=iOS Simulator,name=iPhone 16' \
  -enableCodeCoverage YES -resultBundlePath build/Test.xcresult
```

`putnami observe` finds `build/Test.xcresult` and reads its tests and coverage with `xcrun xcresulttool` (Xcode 16 and later) and `xcrun xccov`. When the repository holds other JUnit reports, it reads only the bundle's coverage, so a converted copy is not counted twice: name the bundle and those reports with `--junit` to read its tests too. To observe on a runner without `xcrun`, such as a Linux job, convert the bundle to JUnit and Cobertura on the macOS runner (`brew install xcresultparser`, then `xcresultparser --output-format junit` and `--output-format cobertura`); otherwise the summary names that conversion and the job still exits 0. For a package on its own, `swift test --enable-code-coverage --xunit-output build/reports/xunit.xml` writes the JUnit report, and `llvm-cov export -format=lcov` the coverage. Each Swift package, each of its targets and each Xcode project is a component; the export gives the targets' dependencies, and a local package the Xcode project references is an edge, so a change to a package marks the packages and the app that use it. Without the export, the summary says how to write it. A test case named `PricingTests.PriceCalculatorTests` lands on the test target `PricingTests`, and a change to `Package.resolved` or `Podfile.lock` is a dependency change of ecosystem `swift` or `cocoapods`.

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

Without a token, the step prints the summary in the job log and writes the evidence document. To also upload the evidence and post the commit status and the summary note on the pull request, give the job these permissions and pass the token of step 6:

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
          token: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}
      - uses: actions/upload-artifact@v4
        if: ${{ !cancelled() }}
        with:
          name: observe-evidence
          path: ${{ steps.observe.outputs.evidence }}
```

The action passes the run's `GITHUB_TOKEN` to `putnami observe` only when it publishes: with a token, unless `publish` is `false`. The `evidence` output is the path of the evidence document; `actions/upload-artifact` keeps it. The action's inputs `junit`, `coverage`, `sbom`, `base-sbom` and `base` pass the flags of the same name, and `channel` picks the release channel of the CLI and the extensions, `stable` by default. A token set in the step's `env` is not read: pass it as the `token` input. Its [README](https://github.com/Putnami/observe-action#readme) lists them.

### 3) GitLab CI or another CI: add a job after your tests

#### GitLab CI

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
    - putnami observe --evidence observe-evidence.json
    # With PUTNAMI_CLOUD_TOKEN set as a masked CI/CD variable, this line uploads the evidence to Putnami Cloud and publishes on the merge request:
    # - putnami observe --evidence observe-evidence.json --upload --publish
  artifacts:
    when: always
    paths:
      - observe-evidence.json
```

`--publish` needs a project access token, because the GitLab job token cannot write notes. In the project, open **Settings > Access tokens**, add a token with the **Developer** role and the `api` scope, then store it as the masked CI/CD variable `PUTNAMI_GITLAB_TOKEN`. A self-managed GitLab works the same way: the job reads its API URL from `CI_API_V4_URL`.

Rename `test` in `needs` if your test job has another name. The image holds Bash, curl, tar, `sha256sum` and Git; any image with those, and Git 2.29 or later, works. `before_script: []` and `cache: []` keep your pipeline's defaults out of this job.

#### Another CI

Jenkins, CircleCI, Buildkite, Azure Pipelines and Bitbucket Pipelines run the same lines at the end of the job that runs your tests, so the reports are still in the checkout. Each snippet runs them when a test fails too. These CIs name no base, so each passes `--base`: on Jenkins, Buildkite and Bitbucket Pipelines the target branch of the pull request the CI names, else `main`; on CircleCI and Azure Pipelines, `main`. Put your default branch in its place. `putnami observe` fetches that branch from `origin` when the checkout lacks it, so the checkout must keep its Git credentials for a private repository: on Azure Pipelines, set `persistCredentials: true` on the `checkout` step; on Jenkins, check out over SSH or with a credential the workspace keeps.

With `--upload`, `putnami observe` reads the repository from the `origin` remote, as on a laptop, and asks GitHub or GitLab for its numeric id: the key a GitHub Actions or GitLab CI job of the same repository sends. Also give the job `GITHUB_TOKEN` or `PUTNAMI_GITLAB_TOKEN`, a token that reads the repository: a private repository needs it, and a public one avoids the GitHub limit on calls without a token, which shared CI runners reach. Without an answer, the summary names the token to set and nothing is uploaded. Store `PUTNAMI_CLOUD_TOKEN`, from [step 6](#6-upload-the-evidence-to-putnami-cloud), as a secret of the CI. Without it, drop `--upload`: the job prints the summary and writes the evidence document.

Jenkins, in the `post` section of the stage that runs your tests, with the token stored as the secret text credential `putnami-cloud-token`:

```groovy
post {
  always {
    withCredentials([string(credentialsId: 'putnami-cloud-token', variable: 'PUTNAMI_CLOUD_TOKEN')]) {
      sh '''
        curl -fsSL https://putnami.dev/install.sh | bash
        export PATH="$HOME/.putnami/bin:$PATH"
        putnami extensions install --user @putnami/observe
        putnami extensions install --user @putnami/cloud
        putnami observe --base "${CHANGE_TARGET:-main}" --evidence observe-evidence.json --upload
      '''
    }
    archiveArtifacts artifacts: 'observe-evidence.json', allowEmptyArchive: true
  }
}
```

CircleCI, as the last steps of the job that runs your tests, with `PUTNAMI_CLOUD_TOKEN` set in the project's environment variables or a context:

```yaml
      - run:
          name: Observe the change
          when: always
          command: |
            curl -fsSL https://putnami.dev/install.sh | bash
            export PATH="$HOME/.putnami/bin:$PATH"
            putnami extensions install --user @putnami/observe
            putnami extensions install --user @putnami/cloud
            putnami observe --base main --evidence observe-evidence.json --upload
      - store_artifacts:
          path: observe-evidence.json
```

Buildkite, at the end of the `command` of the step that runs your tests, with `PUTNAMI_CLOUD_TOKEN` exposed to the step by your secrets plugin or agent hook. Your test command records its exit status in `status` instead of stopping the step, so the lines run when a test fails, and the step ends with that status. `buildkite-agent pipeline upload` replaces each `$VAR` when it uploads the pipeline, so each variable the step reads when it runs is written `$$VAR`:

```yaml
steps:
  - label: ":test_tube: Test"
    command: |
      your-test-command && status=0 || status=$$?
      curl -fsSL https://putnami.dev/install.sh | bash
      export PATH="$$HOME/.putnami/bin:$$PATH"
      putnami extensions install --user @putnami/observe
      putnami extensions install --user @putnami/cloud
      putnami observe --base "$${BUILDKITE_PULL_REQUEST_BASE_BRANCH:-main}" --evidence observe-evidence.json --upload
      exit "$$status"
    artifact_paths:
      - observe-evidence.json
```

Azure Pipelines, as the last steps of the job that runs your tests, with `PUTNAMI_CLOUD_TOKEN` a secret variable of the pipeline, which a script reads only when its `env` maps it:

```yaml
  - script: |
      curl -fsSL https://putnami.dev/install.sh | bash
      export PATH="$HOME/.putnami/bin:$PATH"
      putnami extensions install --user @putnami/observe
      putnami extensions install --user @putnami/cloud
      putnami observe --base main --evidence observe-evidence.json --upload
    displayName: Observe the change
    condition: succeededOrFailed()
    env:
      PUTNAMI_CLOUD_TOKEN: $(PUTNAMI_CLOUD_TOKEN)
  - publish: observe-evidence.json
    artifact: observe-evidence
    condition: succeededOrFailed()
```

Bitbucket Pipelines, in the `after-script` of the step that runs your tests, which runs when a test fails too:

```yaml
        after-script:
          - curl -fsSL https://putnami.dev/install.sh | bash
          - export PATH="$HOME/.putnami/bin:$PATH"
          - putnami extensions install --user @putnami/observe
          - putnami observe --base "${BITBUCKET_PR_DESTINATION_BRANCH:-main}" --evidence observe-evidence.json
        artifacts:
          - observe-evidence.json
```

A Bitbucket Pipelines checkout has its `origin` on `bitbucket.org`, neither GitHub nor GitLab, so the repository has no key Putnami Cloud reads: the job prints the summary and keeps the evidence document, and `--upload` would be refused. The step uses the Linux image of your test step; it needs Bash, curl, tar, `sha256sum` and Git 2.29 or later.

### 4) Read the job log

The step prints a short summary like this one, from a Go repository:

```text
putnami observe: GitHub Actions, github.com/Putnami/demo-observe-go (project 1442100792)
  commit   bd5c750895a5, on pricing-discount, pull request #1
  base     eb60e16d8997, merge base with main from the pull request; the shallow clone was deepened by up to 64 commits
  changed  7 files
  components  1 changed, 1 dependent, 0 unaffected
    changed   pricing [go, go.mod]
    dependent api [go, go.mod], depends on pricing
  changed lines  8 of 10 lines (80.0%) covered
  dependencies  1 change; without an SBOM, their licenses and vulnerabilities are unknown
Next: pass a token from `putnami cloud token create --organization <id> --name ci` as this action's token input to keep the full report
```

A report it cannot find or read is a warning in the same summary, such as `observe.report_missing`. The last line names what keeps the full report: the action's `token` input; in another GitHub Actions job, the organization to name for the ID token exchange or the token to set; in another CI, the token to set; `--upload` once the job holds one, or the report URL once an upload answered it. `--details` prints the changed files, the reports, each component's tests and coverage, and each dependency change instead. The evidence document is the `observe-evidence` artifact of the job.

### 5) Read the pull or merge request

With `--upload --publish`, on a pull or merge request, `putnami observe` sets the commit status `putnami/observe` on the pull request's head commit, or on the commit of the merge request pipeline, with a one-line summary such as `1 changed, 2 dependent components; changed lines 80.0% covered; 1 dependency change`. It also writes one summary note with the components, their tests and coverage, the least covered changed lines and the dependency changes. The status and the note's first link open the full report the upload stored, once the reporter answers its URL; until then they link the CI run. The next run updates the same note instead of adding one.

Without `--upload`, after a failed upload, without the code host token, or on a branch push, the summary says why nothing was posted, and the job still passes. On a pull request from a fork, GitHub gives a read-only `GITHUB_TOKEN` and refuses the post; the job still passes. [Pull requests from forks](#pull-requests-from-forks) relays it from your repository instead.

### 6) Upload the evidence to Putnami Cloud

To send the evidence to Putnami Cloud, store your organization's ingest token as the secret or masked CI/CD variable `PUTNAMI_CLOUD_TOKEN`. Signed in with `putnami observe --open`, run `putnami cloud token create --organization <organization-id> --name ci` anywhere: it needs no Putnami workspace, creates a token with the `delivery.ingest` scope only, and prints it once. It needs the right to manage the organization. The upload names the repository, and its first upload opens the repository's observed workspace in the organization. `putnami cloud token list --organization <organization-id>` lists the tokens without their secrets, and `putnami cloud token revoke <name> --organization <organization-id>` revokes one. On GitHub Actions, the job can skip the secret and exchange its ID token instead, as [below](#on-github-without-a-secret) shows.

On GitHub, pass it to the action as `token: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}`, as step 2 shows. With a token, the action uploads the evidence and publishes unless you set `publish` to `false`, so the job needs the permissions of step 2. On GitLab, use the commented `--upload --publish` line in place of the line above it. An upload that fails is a diagnostic in the summary, and the job still passes.

#### On GitHub, without a secret

A GitHub Actions job can upload with no secret at all: it exchanges the job's own ID token for a five-minute upload token. Your Putnami organization must first trust the repository, or the GitHub organization that owns it, on its observe page in putnami.app. Then commit this file as `.github/workflows/putnami.yml`, with your organization's id in place of `<organization-id>` and your default branch in place of `main`:

```yaml
name: Putnami
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
  id-token: write
jobs:
  observe:
    runs-on: ubuntu-latest
    env:
      PUTNAMI_ORGANIZATION: <organization-id>
    steps:
      - uses: actions/checkout@v4
      - name: Install Putnami
        run: |
          curl -fsSL https://putnami.dev/install.sh | bash
          echo "$HOME/.putnami/bin" >> "$GITHUB_PATH"
      - name: Install the observe and cloud extensions
        run: |
          putnami extensions install --user @putnami/observe
          putnami extensions install --user @putnami/cloud
      - name: Observe the change
        run: putnami observe --upload
```

The workflow runs on each pull request and on each push to the default branch, where `putnami observe` measures the push against the commit before it. It needs no test report: add the reports of [step 1](#1-write-the-reports-your-stack-needs) to the same job, before the last step, when you want tests and coverage in the report. `PUTNAMI_ORGANIZATION` is an id, not a secret, so it sits in the file.

`id-token: write` lets the job ask GitHub for an ID token that names its repository. With `PUTNAMI_ORGANIZATION` set and no `PUTNAMI_CLOUD_TOKEN`, `@putnami/cloud` asks for that token and exchanges it at `auth.putnami.cloud`, which answers an upload token only when the organization trusts the repository, or its owner, for observation. When it does not, the summary names that refusal and the trust to add; without `id-token: write`, it names the missing permission. GitHub withholds the ID token from a pull request from a fork in most repositories, so such a run uploads nothing: relay it as the next section says. As always, an upload that fails is a warning, and the job still passes. A `PUTNAMI_CLOUD_TOKEN` set in the job wins over the exchange.

#### Pull requests from forks

GitHub runs the workflow of a pull request from a fork with no ID token and a read-only `GITHUB_TOKEN`, and keeps your secrets from it, so that run can neither upload nor post. It keeps its evidence document as an artifact instead, and a second workflow of your repository relays it with your repository's credentials.

The workflow that runs your tests on `pull_request`, here named `Test`, keeps the step of step 2 without a token, and keeps its document:

```yaml
      - id: observe
        uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
      - uses: actions/upload-artifact@v4
        if: ${{ !cancelled() }}
        with:
          name: observe-evidence
          path: ${{ steps.observe.outputs.evidence }}
```

Then commit this workflow as `.github/workflows/putnami-relay.yml` on your default branch, where GitHub reads `workflow_run` workflows, with the name of your test workflow in `workflows` and your organization's id in place of `<organization-id>`:

```yaml
name: Putnami relay
on:
  workflow_run:
    workflows: [Test]
    types: [completed]
permissions:
  actions: read
  id-token: write
  statuses: write
  pull-requests: write
jobs:
  relay:
    if: github.event.workflow_run.event == 'pull_request'
    runs-on: ubuntu-latest
    env:
      PUTNAMI_ORGANIZATION: <organization-id>
    steps:
      - uses: Putnami/observe-action@v1
        with:
          from-artifact: observe-evidence
```

When the test run completes, the step downloads its `observe-evidence` artifact and runs `putnami observe --from-evidence` on it with `--upload --publish`, with this job's ID token and `GITHUB_TOKEN`. `actions: read` lets it download the artifact; pass `token: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}` instead of `PUTNAMI_ORGANIZATION` to upload with a workspace token. The pull request gets the status and the note as a pull request from a branch does, and they link the fork's run. A branch of your repository goes the same way, so one setup serves both.

The document comes from the fork, so `putnami observe` checks it before it relays it. It refuses a document over 64 MiB, one that does not validate, or one that names another repository, pull request or commit than the run that triggered the job, with the warning `observe.evidence_refused`, and the job still passes: the commit must be the run's head commit, or the merge commit GitHub keeps for the pull request, as the GitHub API answers, and the base must be that merge commit's first parent or an ancestor of the head. It takes the repository, the pull request and the run from the job and the event, never from the document. The note quotes every text of the document as code, so a test name renders no link, mention or HTML, and the log prints only counts and observe's own diagnostics. The relay job checks out nothing and runs nothing the artifact holds: keep it that way, since it holds your repository's credentials. GitHub lists no pull request in the event of a fork's run, so observe finds it with `GITHUB_TOKEN` among the open pull requests of your repository, by the fork's owner, branch and head commit; when none matches, as after a new push to the fork or once the pull request closed, it relays nothing. The action hands `GITHUB_TOKEN` to `putnami observe` for these reads, even with `publish: false`.

#### On GitLab, without a secret

A job on gitlab.com can upload with no secret too: it exchanges an ID token GitLab gives the job for a five-minute upload token. Your Putnami organization must first trust the project, or the group that owns it, on its observe page in putnami.app. Then give the observe job of [step 3](#gitlab-ci) your organization's id and an ID token whose audience is `https://auth.putnami.cloud`, and upload:

```yaml
observe:
  # ...the job of step 3, with these additions:
  variables:
    PUTNAMI_ORGANIZATION: <organization-id>
  id_tokens:
    PUTNAMI_ID_TOKEN:
      aud: https://auth.putnami.cloud
  script:
    - curl -fsSL https://putnami.dev/install.sh | bash
    - export PATH="$HOME/.putnami/bin:$PATH"
    - putnami extensions install --user @putnami/observe
    - putnami extensions install --user @putnami/cloud
    - putnami observe --evidence observe-evidence.json --upload
```

`PUTNAMI_ORGANIZATION` is an id, not a secret, so it sits in the file. GitLab signs the ID token before the job starts, naming the project, its group and the job. With `PUTNAMI_ORGANIZATION` set and no `PUTNAMI_CLOUD_TOKEN`, `@putnami/cloud` exchanges `PUTNAMI_ID_TOKEN` at `auth.putnami.cloud`, which answers an upload token only when the organization trusts the project, or its group, for observation. When it does not, the summary names that refusal and the trust to add; without `PUTNAMI_ID_TOKEN`, it names the `id_tokens` entry to add. Putnami Cloud verifies the ID tokens of gitlab.com only: on a self-managed GitLab, set `PUTNAMI_CLOUD_TOKEN` as step 6 says. `@putnami/cloud` sends no ID token whose issuer is not gitlab.com or whose audience is not `https://auth.putnami.cloud`, and the summary names the one it read. A merge request from a fork runs in the fork's project, whose ID token names the fork, so it uploads nothing unless your organization trusts that project too. Add `--publish` and its `PUTNAMI_GITLAB_TOKEN`, as step 3 says, to also post on the merge request. A `PUTNAMI_CLOUD_TOKEN` set in the job wins over the exchange.

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

Without `--base-sbom`, each dependency gets its state at the head and the summary says the base is unknown. Without `--sbom`, `putnami observe` reads an SBOM it finds in the checkout, in a `.json` file whose name contains `bom`, `cdx`, `cyclonedx` or `spdx`. The short summary counts the licence changes and the vulnerabilities introduced and fixed, and with `--details` it gives each change one more line:

```text
  dependencies  3 changes, SBOM head CycloneDX 1.5, base CycloneDX 1.5
    upgraded   lodash 4.17.20 -> 4.17.21, in package-lock.json
               fixes 2 (GHSA-35jh-r3h4-6jhm high, GHSA-29mw-wpgm-hmr9 medium), introduces 0, keeps 3; license MIT
    added      zod 3.23.8, in package-lock.json
               introduces 1 (GHSA-mrhx-c7xc-m3w5 high); license MIT
```

### 8) See the decisions that govern a change

If your team keeps its decisions as MADR files (Markdown Architectural Decision Records) under `docs/decisions` or `docs/adr`, `putnami observe` lists, with no other change, the decisions whose scope covers a changed file and the records the change adds, edits or deletes. A record gives its scope in its front matter or in a `## Scope` section:

```markdown
---
status: accepted
date: 2025-03-04
scope:
  - src/payments/
---

# Payments own the ledger
```

Records kept elsewhere are named on the observe line, such as `putnami observe --decisions architecture/decisions`. With `--details`, the summary lists each decision with its status, and a change that deletes an accepted decision gets a warning in every summary:

```text
  decisions  2 decisions
    accepted   docs/decisions/0001-charges-are-synchronous.md: Charges are synchronous (deleted by the change)
    accepted   docs/decisions/0002-payments-own-the-ledger.md: Payments own the ledger (governs 1 changed file)
  warning observe.decision_deleted: the change deletes the accepted decision docs/decisions/0001-charges-are-synchronous.md (Charges are synchronous)
```

### 9) Show your architecture rules

When your CI checks its architecture with ArchUnit, deptrac or import-linter, `putnami observe` shows each rule the checker found broken, with each violation, its file, its line and its component. Keep the checker's report where the observe step can read it; you rewrite no rule.

- **ArchUnit** runs as JUnit tests: nothing to add. A test class that imports `com.tngtech.archunit` is read as rules, not tests. Name a class that does not with `--archunit-classes '*ArchitectureTest'`.
- **deptrac**: write its JUnit report, `vendor/bin/deptrac analyse --formatter=junit --output=deptrac.xml || true`.
- **import-linter**: save its output, `lint-imports > import-linter.txt || true`.

`|| true` keeps the checker from stopping the job before the observe step; keep a separate step that runs the checker without it when a broken rule must fail the job. `putnami observe` finds the deptrac report by its content, and the import-linter output in a `.txt` file whose name contains `lint`. Name them with `--architecture deptrac.xml,import-linter.txt` when they lie elsewhere. With `--details`, the summary then counts the rules:

```text
  architecture  2 rules, 1 broken
    broken    import-linter: Shared imports no service (1 violation)
      shared shared/src/shared/money.py:3: shared.money -> api.main (l.3)
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
- [How `putnami observe` explains a change](/docs/tooling-&-workspace/observe/change-analyses): components, tests, changed-line coverage, dependency changes and decision records.
- [Architecture rules](/docs/tooling-&-workspace/observe/architecture): how the ArchUnit, deptrac and import-linter results are read and placed.
- [Reports](/docs/tooling-&-workspace/observe/reports): what each test, coverage and SBOM format gives.

You now have a CI job that records which files a change touched and which tests and coverage reports it produced, as an artifact you can keep or upload to Putnami Cloud.
