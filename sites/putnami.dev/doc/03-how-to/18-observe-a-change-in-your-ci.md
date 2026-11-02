# Observe a change in your CI

You will add one step to a GitHub Actions workflow, or one job to a GitLab CI pipeline, that reads what your tests already produced and writes a change evidence document: which commit was built, what it is measured against, which files changed, and which test and coverage reports the job wrote. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

`putnami observe` comes with the `@putnami/observe` extension. It is `experimental` and, as of November 2026, published on the `canary` channel only, so every command below names `canary`.

## What the snippets install

| Line | What it does |
|---|---|
| `curl -fsSL https://putnami.dev/install.sh \| bash -s -- --version canary` | Installs the canary Putnami CLI into `~/.putnami/bin`. The stable CLI 0.3.0 cannot load the canary `@putnami/cloud`. |
| `export PATH="$HOME/.putnami/bin:$PATH"` | Makes `putnami` reachable in the rest of the step. A script piped into `bash` cannot change the `PATH` of the shell that runs it. |
| `putnami extensions install --user @putnami/observe@canary` | Installs the extension in the user scope, so its command runs outside any workspace. `--user` takes one extension per command. |
| `putnami extensions install --user @putnami/cloud@canary` | Installs the extension that uploads the evidence to Putnami Cloud. |
| `putnami observe --evidence observe-evidence.json --publish` | Prints the summary, writes the evidence document, and posts a commit status and a summary note on the pull or merge request. It exits `0` whatever it observes: only a usage error, or an evidence file it cannot write, fails it. |

`putnami observe` finds the base of the change from the CI: the pull request base on GitHub, the merge request diff base on GitLab, and on a branch push the commit before the push. It deepens a shallow clone itself, so you do not change `fetch-depth` or `GIT_DEPTH`. It finds JUnit, LCOV, Cobertura, Clover and Go cover reports by their content, anywhere in the checkout except `.git`, `node_modules` and `vendor`.

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
          curl -fsSL https://putnami.dev/install.sh | bash -s -- --version canary
          export PATH="$HOME/.putnami/bin:$PATH"
          putnami extensions install --user @putnami/observe@canary
          putnami extensions install --user @putnami/cloud@canary
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
    - curl -fsSL https://putnami.dev/install.sh | bash -s -- --version canary
    - export PATH="$HOME/.putnami/bin:$PATH"
    - putnami extensions install --user @putnami/observe@canary
    - putnami extensions install --user @putnami/cloud@canary
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

## Learn more

- [What `putnami observe` reads](https://github.com/putnami/putnami/blob/main/tooling/observe/doc/01-observe.md): every flag, how the base is found, and how reports are discovered.
- [The `@putnami/observe` README](https://github.com/putnami/putnami/blob/main/tooling/observe/README.md): the questions it answers and how it reads Nx, npm, pnpm, yarn and Go repositories.

You now have a CI job that records which files a change touched and which tests and coverage reports it produced, as an artifact you can keep or upload to Putnami Cloud.
