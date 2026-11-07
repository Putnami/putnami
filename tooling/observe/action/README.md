# Putnami Observe

Explain each change from the test and coverage reports your job already wrote. One step prints, in the job log, which commit the change is measured against, which files changed, which parts of the repository they touch and depend on, which tests passed or failed, how much of the changed lines they covered, and which dependency versions moved. With a token, it also uploads that evidence to Putnami Cloud and posts a commit status and a summary note on the pull request.

The step never fails your job because of what it observes. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

## Use it

Add the step to the job that runs your tests, after the step that writes the reports:

```yaml
      - uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
```

`if: ${{ !cancelled() }}` runs the step when a test fails too: a failed test is part of the evidence. Keep your `actions/checkout` step as it is: the action deepens a shallow clone itself to find the base of the change, so `fetch-depth: 1` works.

With a Putnami Cloud workspace token, stored as the secret `PUTNAMI_CLOUD_TOKEN`, the step also uploads the evidence and posts on the pull request:

```yaml
    permissions:
      contents: read
      statuses: write
      pull-requests: write
    steps:
      # ... your checkout, build and test steps ...
      - uses: Putnami/observe-action@v1
        if: ${{ !cancelled() }}
        with:
          token: ${{ secrets.PUTNAMI_CLOUD_TOKEN }}
```

## Permissions

The action reads the run's `GITHUB_TOKEN` only when `publish` is on, to set the commit status `putnami/observe` and write one summary note on the pull request. Then the job needs `statuses: write` and `pull-requests: write`. With `publish` off, it uses no GitHub token and needs no permission beyond reading the checkout. On a pull request from a fork, GitHub gives a read-only token: the post is refused and the step still passes.

## Inputs

| Input | Default | Meaning |
|---|---|---|
| `token` | empty | Putnami Cloud workspace token with the `delivery.ingest` scope. When set, the step installs `@putnami/cloud` and uploads the evidence. |
| `publish` | `true` with a token, `false` without | `true` posts the commit status and the summary note on the pull request; `false` does not. |
| `junit` | discovery | Comma-separated JUnit XML or TRX paths or glob patterns. Empty finds the reports by their content. |
| `coverage` | discovery | Comma-separated LCOV, Cobertura, Clover, JaCoCo or Go cover paths or glob patterns. |
| `sbom` | discovery | The software bill of materials (SBOM) of the commit, CycloneDX or SPDX JSON. |
| `base-sbom` | empty | The SBOM of the base. Without it, the base state of each dependency is unknown. |
| `base` | the pull request or push | The commit or branch the change is measured against. |
| `channel` | `stable` | Release channel or exact version of the Putnami CLI and its extensions. |

## Outputs

| Output | Meaning |
|---|---|
| `evidence` | Path of the change evidence document the step wrote, under `RUNNER_TEMP`. |

To keep the document, upload it as an artifact:

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

## Runners

The action runs on Linux and macOS runners, such as `ubuntu-latest` and `macos-latest`, and needs the network to install the Putnami CLI from `https://putnami.dev/install.sh`. On a Windows runner it fails with a message.

## Learn more

[Observe a change in your CI](https://putnami.dev/docs/how-to/observe-a-change-in-your-ci) says which reports each stack writes, and [Observe](https://putnami.dev/docs/tooling-&-workspace/observe) lists every CI, repository tool, report format, lockfile and SBOM format the step reads.

The source of this action lives in [Putnami/putnami](https://github.com/Putnami/putnami/tree/main/tooling/observe/action); this repository receives each release.
