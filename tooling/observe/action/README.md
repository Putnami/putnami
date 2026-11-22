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

With your Putnami organization's ingest token, stored as the secret `PUTNAMI_CLOUD_TOKEN`, the step also uploads the evidence and posts on the pull request:

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

## Pull requests from forks

GitHub runs the workflow of a pull request from a fork with no ID token and a read-only `GITHUB_TOKEN`, so that run can neither upload nor post. Keep its evidence as an artifact, and let a second workflow of your repository relay it. In the workflow that runs your tests on `pull_request`, keep the step without a token, and keep its document:

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

Then commit this second workflow on your default branch, with the name of the first workflow in `workflows`:

```yaml
name: Putnami Observe relay
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

With `from-artifact`, the step downloads that artifact from the run that triggered the workflow, then runs `putnami observe --from-evidence` on its `observe-evidence.json` with `--upload`, and with `--publish` unless `publish` is `false`. It uploads with the job's ID token, which `PUTNAMI_ORGANIZATION` exchanges, or with the `token` input. It checks out nothing and runs nothing the artifact holds: never add a checkout of the fork's code to this job, which holds your repository's credentials. It hands the run's `GITHUB_TOKEN` to `putnami observe`, which reads the pull request and the commits with it even when `publish` is `false`. `putnami observe` refuses a document that names another repository, pull request or commit than the run's, relays nothing from a fork whose pull request it does not find, and quotes every text of the document as code in the note. It relays pull requests from branches of your repository the same way. Outside a `workflow_run` job, when the download failed, or when the run kept no `observe-evidence.json`, the step warns and passes.

## Permissions

The action hands the run's `GITHUB_TOKEN` to `putnami observe` with `from-artifact` set, or when `token` is set and `publish` is on, to set the commit status `putnami/observe` and write one summary note on the pull request. Then the job needs `statuses: write` and `pull-requests: write`. Without `from-artifact`, with `publish` off or no `token`, it uses no GitHub token and needs no permission beyond reading the checkout. The tokens reach `putnami observe` only, never the installer. A `PUTNAMI_CLOUD_TOKEN` or `GITHUB_TOKEN` set in the step's `env` is not read: pass the Putnami token as the `token` input. On a pull request from a fork, GitHub gives a read-only token: the post is refused and the step still passes; [relay it](#pull-requests-from-forks) instead. With `from-artifact`, the job also needs `actions: read` to download the artifact, and `id-token: write` to upload without the `token` input.

## Inputs

| Input | Default | Meaning |
|---|---|---|
| `token` | empty | Putnami Cloud token with the `delivery.ingest` scope: the organization's ingest token that `putnami cloud token create --organization <id> --name ci` creates, with no Putnami workspace. When set, the step installs `@putnami/cloud` and uploads the evidence. |
| `publish` | `true` with a token, `false` without | `true` posts the commit status and the summary note on the pull request, which link to the report the upload stored, so it needs `token`; `false` does not post. |
| `junit` | discovery | Comma-separated JUnit XML or TRX paths or glob patterns. Empty finds the reports by their content. |
| `coverage` | discovery | Comma-separated LCOV, Cobertura, Clover, JaCoCo or Go cover paths or glob patterns. |
| `sbom` | discovery | The software bill of materials (SBOM) of the commit, CycloneDX or SPDX JSON. |
| `base-sbom` | empty | The SBOM of the base. Without it, the base state of each dependency is unknown. |
| `base` | the pull request or push | The commit or branch the change is measured against. |
| `from-artifact` | empty | In a `workflow_run` job, the artifact that holds the evidence document a pull request run kept. The step relays it instead of observing the checkout, and then takes no `junit`, `coverage`, `sbom`, `base-sbom` or `base` input. See [Pull requests from forks](#pull-requests-from-forks). |
| `channel` | `stable` | Release channel of the Putnami CLI and its extensions, such as `stable` or `canary`. `stable` reads the `latest` channel, as `putnami upgrade` does. A version is refused: the CLI and the extensions do not share one version. |

## Outputs

| Output | Meaning |
|---|---|
| `evidence` | Path of the change evidence document the step wrote, under `RUNNER_TEMP`, or relayed with `from-artifact`. |

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
