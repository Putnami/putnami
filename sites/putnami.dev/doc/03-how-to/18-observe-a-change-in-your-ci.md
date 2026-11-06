# Observe a change in your CI

You will add one step to a GitHub Actions workflow, or one job to a GitLab CI pipeline, that reads what your tests already produced and writes a change evidence document: which commit was built, what it is measured against, which files changed, and which test and coverage reports the job wrote. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

`putnami observe` comes with the `@putnami/observe` extension. It is `experimental`: being on the stable channel is not a compatibility promise.

## What the snippets install

| Line | What it does |
|---|---|
| `curl -fsSL https://putnami.dev/install.sh \| bash` | Installs the stable Putnami CLI into `~/.putnami/bin`. |
| `export PATH="$HOME/.putnami/bin:$PATH"` | Makes `putnami` reachable in the rest of the step. A script piped into `bash` cannot change the `PATH` of the shell that runs it. |
| `putnami extensions install --user @putnami/observe` | Installs the extension in the user scope, so its command runs outside any workspace. `--user` takes one extension per command. |
| `putnami extensions install --user @putnami/cloud` | Installs the extension that uploads the evidence to Putnami Cloud. |
| `putnami observe --evidence observe-evidence.json --publish` | Prints the summary, writes the evidence document, and posts a commit status and a summary note on the pull or merge request. It exits `0` whatever it observes: only a usage error, or an evidence file it cannot write, fails it. |

`putnami observe` finds the base of the change from the CI: the pull request base on GitHub, the merge request diff base on GitLab, and on a branch push the commit before the push. It deepens a shallow clone itself, so you do not change `fetch-depth` or `GIT_DEPTH`. It finds JUnit, LCOV, Cobertura, Clover and Go cover reports by their content, anywhere in the checkout except `.git`, `node_modules` and `vendor`.

In a Python repository, have pytest write both reports, with pytest-cov installed:

```bash
pytest --junitxml=junit.xml --cov --cov-report=xml
```

`putnami observe` places each test case and each covered file on the project that holds it: a member of the uv workspace, or a directory whose `pyproject.toml` has a `[project]` or `[tool.poetry]` table. In a uv workspace, run it from the root as `uv run --all-packages pytest …`, so every member is installed. pytest names each test case from its file's path relative to its `rootdir`: if pytest takes another `rootdir`, for example a project's directory that holds the pytest configuration, add `--rootdir=.` so the names stay relative to the repository root. A `uv.lock` or `poetry.lock` change is a dependency change of ecosystem `pypi`.

In a Pants repository, write the export of the target graph and run the tests with their reports, before the observe step:

```bash
pants peek :: > pants-graph.json
pants test --test-report --use-coverage --coverage-py-report=xml ::
```

`pants peek` lists every target with the dependencies Pants infers from imports, so `putnami observe` knows that a change to a library marks the code that imports it. Each directory of BUILD targets is a component; without the export, the components have no edge, and the summary says how to write it. The JUnit reports under `dist/test/reports` and the coverage under `dist/coverage` land on the components, and a change to a lockfile that `[python.resolves]` in `pants.toml` names is a dependency change of ecosystem `pypi`. Write the export elsewhere with `--pants-graph <path>`.

In an iOS or Swift repository, write each package's description and convert the `.xcresult` bundle to JUnit and Cobertura, before the observe step. The same lines run in a GitHub Actions job on `runs-on: macos-15` and in a GitLab job on a macOS runner (`tags: [saas-macos-medium-m1]`), with xcbeautify and xcresultparser installed (`brew install xcbeautify xcresultparser`):

```bash
(cd Packages/Pricing && swift package describe --type json > swiftpm.json)
set -o pipefail
xcodebuild test -workspace Shop.xcworkspace -scheme Shop -destination 'platform=iOS Simulator,name=iPhone 16' \
  -enableCodeCoverage YES -resultBundlePath build/Test.xcresult | xcbeautify --report junit --report-path build/reports
xcresultparser --output-format cobertura --project-root . build/Test.xcresult > build/reports/coverage.xml
```

For a package on its own, `swift test --enable-code-coverage --xunit-output build/reports/xunit.xml` writes the JUnit report, and `llvm-cov export -format=lcov` the coverage. `putnami observe` reads no `.xcresult` itself. Each Swift package, each of its targets and each Xcode project is a component; the export gives the targets' dependencies, and a local package the Xcode project references is an edge, so a change to a package marks the packages and the app that use it. Without the export, the summary says how to write it. A test case named `PricingTests.PriceCalculatorTests` lands on the test target `PricingTests`, and a change to `Package.resolved` or `Podfile.lock` is a dependency change of ecosystem `swift` or `cocoapods`.

The install lines need the network: when the installer or the registry cannot be reached, the GitHub step or the GitLab job fails.

## Steps

### 1) GitHub Actions: add a step to your test job

Give the job that runs your tests these permissions, so `--publish` can post on the pull request:

```yaml
    permissions:
      contents: read
      statuses: write
      pull-requests: write
```

Then add these two steps to that job, after the step that writes the reports:

```yaml
      - name: Observe the change
        if: ${{ !cancelled() }}
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          # Uncomment once an ingest token is set, to upload the evidence to Putnami Cloud.
          # PUTNAMI_CLOUD_TOKEN: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}
        run: |
          curl -fsSL https://putnami.dev/install.sh | bash
          export PATH="$HOME/.putnami/bin:$PATH"
          putnami extensions install --user @putnami/observe
          putnami extensions install --user @putnami/cloud
          putnami observe --evidence observe-evidence.json --publish
          # putnami observe --evidence observe-evidence.json --publish --upload
      - uses: actions/upload-artifact@v4
        if: ${{ !cancelled() }}
        with:
          name: observe-evidence
          path: observe-evidence.json
```

`if: ${{ !cancelled() }}` runs the step when a test fails too: a failed test is part of the evidence. The job needs a Linux or macOS runner, such as `ubuntu-latest`. `GITHUB_TOKEN` is the token GitHub gives every run; a workflow passes it to a step through `env`.

### 2) GitLab CI: add a job after your test job

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

### 3) Read the job log

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

### 4) Read the pull or merge request

On a pull or merge request, `--publish` sets the commit status `putnami/observe` on the pull request's head commit, or on the commit of the merge request pipeline, with a one-line summary such as `1 changed, 2 dependent components; changed lines 80.0% covered; 1 dependency change`. It also writes one summary note with the components, their tests and coverage, the least covered changed lines and the dependency changes. The next run updates the same note instead of adding one.

Without the token, or on a branch push, the summary says why nothing was posted, and the job still passes. On a pull request from a fork, GitHub gives a read-only `GITHUB_TOKEN` and refuses the post; the job still passes.

### 5) Upload the evidence to Putnami Cloud

To send the evidence to Putnami Cloud, store a workspace token with the `delivery.ingest` scope as the secret or masked CI/CD variable `PUTNAMI_CLOUD_TOKEN`. Then use the commented `--upload` line in place of the line above it, and on GitHub uncomment the `env` block. The token names the workspace. An upload that fails is a diagnostic in the summary, and the job still passes.

### 5) Add licences and known vulnerabilities from an SBOM

A software bill of materials (SBOM) lists the packages of a build with their licences and known vulnerabilities. `putnami observe` reads one the job wrote, CycloneDX 1.4 to 1.6 or SPDX 2.3 in JSON, and gives each changed dependency what it lists: the vulnerabilities the change fixes or introduces, and the licences before and after. It queries no vulnerability database itself.

On GitHub Actions, put this step before the observe step. It writes an SBOM of the head and one of the base with [osv-scanner](https://github.com/google/osv-scanner), which reads the lockfiles of Node, Composer, Go and Python projects, `uv.lock` and `poetry.lock` included. The base is the pull request base, or on a push the commit before the push:

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

Then name both documents on the observe line:

```bash
putnami observe --evidence observe-evidence.json --sbom "$RUNNER_TEMP/head.cdx.json" --base-sbom "$RUNNER_TEMP/base.cdx.json"
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

## Learn more

- [What `putnami observe` reads](https://github.com/putnami/putnami/blob/main/tooling/observe/doc/01-observe.md): every flag, how the base is found, and how reports and SBOMs are discovered.
- [The `@putnami/observe` README](https://github.com/putnami/putnami/blob/main/tooling/observe/README.md): the questions it answers and how it reads Nx, Pants, npm, pnpm, yarn, Composer, Go, Python, Swift and Xcode repositories.

You now have a CI job that records which files a change touched and which tests and coverage reports it produced, as an artifact you can keep or upload to Putnami Cloud.
