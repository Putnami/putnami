# Putnami Observe

Explain each change from the test and coverage reports your pipeline already wrote. One job prints, in the job log, which commit the change is measured against, which files changed, which parts of the repository they touch and depend on, which tests passed or failed, how much of the changed lines they covered, and which dependency versions moved. On gitlab.com, with your Putnami organization id, it also uploads that evidence to Putnami Cloud with the job's ID token, with no Putnami secret in your project, and posts a commit status and a summary note on the merge request.

The job never fails your pipeline because of what it observes. Your repository needs no Putnami workspace, and your pipeline keeps its own build and test commands.

## Use it

Add the component to `.gitlab-ci.yml`. It adds the job `observe`, which runs after the job named `test`, when the pipeline has it, and reads the reports from its artifacts, so that job must keep its reports as artifacts with `when: always`:

```yaml
include:
  - component: gitlab.com/putnami/observe/observe@1
```

`@1` runs the newest `1.x.y` release. The job runs when a test fails too: a failed test is part of the evidence. Keep your clone settings as they are: the job deepens a shallow clone itself to find the base of the change, so `GIT_DEPTH` stays.

To upload the evidence and post on the merge request, first trust the project, or the group that owns it, on your organization's observe page in putnami.app. Then give the component your organization's id:

```yaml
include:
  - component: gitlab.com/putnami/observe/observe@1
    inputs:
      organization: <organization-id>
```

The organization id is not a secret. GitLab signs an ID token for the job, naming the project, its group, the pipeline and the job, and the job exchanges it at `auth.putnami.cloud` for a five-minute upload token of your organization.

The note on the merge request needs a project access token, because the GitLab job token cannot write notes. In the project, open **Settings > Access tokens**, add a token with the **Developer** role and the `api` scope, then store it as the masked CI/CD variable `PUTNAMI_GITLAB_TOKEN`. Without it, the job uploads, and its summary says why nothing was posted.

## Inputs

| Input | Default | What it does |
|---|---|---|
| `stage` | `test` | Stage of the job: the stage of your tests or a later one. |
| `needs` | the job `test`, optional | Jobs whose artifacts hold the reports. The job runs after them, even when one failed. A job you name that a pipeline lacks stops that pipeline: name it as `{job: <name>, optional: true}` when its rules leave it out of some pipelines. |
| `image` | `buildpack-deps:bookworm-scm` | Image of the job. It needs Bash, curl, tar, `sha256sum` and Git 2.29 or later. |
| `organization` | empty | Your Putnami organization id. On gitlab.com, the job exchanges its ID token for an upload token and uploads. |
| `publish` | `true` | When the job uploads, also post the commit status and the summary note. `false` uploads only. |
| `channel` | `stable` | Release channel of the Putnami CLI and its extensions, such as `stable` or `canary`. A version is refused. |
| `junit`, `coverage`, `sbom`, `base-sbom`, `base` | empty | Pass the flag of the same name to `putnami observe`. Empty, it finds the reports in the checkout and the base from the merge request. |

The job keeps `observe-evidence.json`, the change evidence document, as an artifact. To give it rules, such as running in merge request pipelines only, add `observe: rules:` to your `.gitlab-ci.yml`: GitLab merges it into the component's job. Remove an `observe` job you wrote by hand before, since it merges the same way.

## Tokens

The job declares one ID token, `PUTNAMI_ID_TOKEN`, whose only audience is `https://auth.putnami.cloud`. It reaches `putnami observe` only for the exchange: with the `organization` input set and no `PUTNAMI_CLOUD_TOKEN`. `PUTNAMI_GITLAB_TOKEN` reaches it only when the job uploads and publishes. The installer and the extension installs see none of the tokens GitLab and the component give the job; your other CI/CD variables reach every command of the job.

A self-managed GitLab includes the component once an administrator mirrors this project on the instance. There, Putnami Cloud cannot verify the instance's ID tokens: store your organization's ingest token, from `putnami cloud token create --organization <organization-id> --name ci`, as the masked CI/CD variable `PUTNAMI_CLOUD_TOKEN`. The job then uploads with it, and it wins over the exchange on gitlab.com too.

## Learn more

[Observe a change in your CI](https://putnami.dev/docs/how-to/observe-a-change-in-your-ci) gives every CI, the reports `putnami observe` reads, and what leaves the job.

The source of this component, its tests and its releases live in [github.com/Putnami/putnami](https://github.com/Putnami/putnami/tree/main/tooling/observe/gitlab). A release replaces this project's files.
