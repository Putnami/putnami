# Start from an existing repository

You will put Putnami on a repository your team already has, without rewriting its build or its CI. The path has four steps, and each one stands on its own: you can stop after any of them and keep what it gave you. The first two add one step to your CI and leave the rest of the repository as it is; the last two turn it into a Putnami workspace.

| Step | What you get | What it asks of the team |
|---|---|---|
| 1. Observe a change on your laptop, then in your CI | Which parts of the repository a change touches, which depend on them, and what its tests and coverage say | One person installs the CLI; one step in a GitHub Actions job, or one job in a GitLab CI pipeline |
| 2. Upload the evidence and get a note on each pull request | A full report of each change, and a status and a note on its pull or merge request | A Putnami Cloud account, one CI secret minted from a Putnami workspace linked to Putnami Cloud, and write access to statuses and pull requests |
| 3. Bring the specs you already have | Each requirement of your Spec Kit, OpenSpec, Kiro or Gherkin specs, and whether a test proves it | A Putnami workspace, and one check named per requirement |
| 4. Go further with Putnami Cloud | A verdict on each pull request, reviews, and a merge from the change page | The workspace linked to Putnami Cloud, the GitHub repository connected, and hosted CI enabled |

## 1) Observe a change on your laptop, then in your CI

On the branch of a change, install the CLI and two extensions in your user scope, then run `putnami observe` in your checkout:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
putnami extensions install --user @putnami/observe
putnami extensions install --user @putnami/cloud
putnami observe
```

It measures the change against the default branch of your `origin` remote, prints a short summary, and sends nothing off your machine. In a repository whose CI kept nothing, `putnami observe --history` analyzes its last 14 days of commits instead. Then add the step to the job that runs your tests, so every pull or merge request gets the same answer from the reports your tests already write.

**What it asks:** one person, a few minutes on a laptop, and one CI change: the step `Putnami/observe-action@v1` on GitHub Actions, or one job on GitLab CI. Your build and test commands stay as they are; some stacks add a flag that writes a test or coverage report. Whatever the change holds, a failed test included, the step passes; a failed install fails it.

**Follow:** [Observe a change in your CI](/docs/how-to/observe-a-change-in-your-ci), from "Start on your laptop" to step 4. Its section [What leaves the CI](/docs/how-to/observe-a-change-in-your-ci#what-leaves-the-ci) lists what the later steps send, and to whom.

## 2) Upload the evidence and get a note on each pull request

On your laptop, `putnami observe --open` signs you in to Putnami Cloud in your browser, uploads the evidence and opens the full report. In the CI, a token lets the step upload the evidence of every change and post a commit status and one note on its pull or merge request, which link to the full report.

**What it asks:** a Putnami Cloud account, for `putnami observe --open`, which opens the observed workspace in your organization; for a private repository, also a `GITHUB_TOKEN` or `PUTNAMI_GITLAB_TOKEN` that reads it. For the CI, your organization's ingest token, stored as the CI secret `PUTNAMI_CLOUD_TOKEN`: `putnami cloud token create --organization <organization-id> --name ci` creates it, signed in, with no Putnami workspace, and prints it once. On GitHub Actions, the job can instead exchange its own ID token, with `id-token: write` and `PUTNAMI_ORGANIZATION`, once the organization trusts the repository. The CI also needs permission to post on the code host. On GitHub, the job gets `statuses: write` and `pull-requests: write`. On GitLab, a project access token with the `api` scope, stored as `PUTNAMI_GITLAB_TOKEN`. A failed upload or post is a warning in the summary, and the job still passes.

**Follow:** [Read the pull or merge request](/docs/how-to/observe-a-change-in-your-ci#5-read-the-pull-or-merge-request) and [Upload the evidence to Putnami Cloud](/docs/how-to/observe-a-change-in-your-ci#6-upload-the-evidence-to-putnami-cloud).

## 3) Bring the specs you already have

`putnami observe` already lists the Spec Kit, OpenSpec, Kiro and Gherkin specs it finds in its summary. To know, for each requirement, whether a test proves it, and to fail a change that loses a proven one, the repository becomes a Putnami workspace and reads those specs where they are. The Markdown or `.feature` files stay the one home of the prose.

**What it asks:** a `putnami.workspace.json` at the root of the repository. A repository that Bazel, moon, Nx, Pants, Rush or Turborepo builds gets one from its tool, and keeps its build; any other repository starts from the reference of its language. Then one feature entry per spec, and one check named per requirement. Specs start in `report` mode, which changes no exit code; you choose when to enforce.

**Follow:** [Adopt Putnami on an existing monorepo](/docs/how-to/adopt-putnami-on-an-existing-monorepo) or [Languages and monorepo tools](/docs/languages) for the workspace, then [Bring your Spec Kit, OpenSpec, Kiro or Gherkin specs](/docs/how-to/bring-your-external-specs).

## 4) Go further with Putnami Cloud

With the workspace in place, hosted CI runs its pipeline on each pull request. The `Putnami Verdict` check says what the change newly proves, what it loses and what waits for a person; a lost requirement blocks the merge until a person accepts it. Reviews read immutable pull-request heads, and on the change page in putnami.app a workspace admin approves the declarations a change touches and a workspace manager merges it.

**What it asks:** `putnami cloud setup` once, to link the repository to a Putnami Cloud workspace; the Putnami GitHub App on the repository, with `putnami cloud source connect`; `putnami cloud ci enable`; and a `putnami.ci.json` at the root that names the pipeline. Reviews need a workspace subscription that includes them.

**Follow:**

- [Set up the workspace](/docs/platform/setup) and [Source](/docs/platform/source) to link and connect the repository.
- [Hosted CI](/docs/platform/ci) to enable the pipeline, and [Read what a change proved and lost](/docs/platform/ci#read-what-a-change-proved-and-lost) for the verdict.
- [Intelligence and reviews](/docs/platform/intelligence) for reviews.
- [Decide a change in the app](/docs/platform/ci#decide-a-change-in-the-app) to approve and merge from the change page.
- [Connect Claude or Codex](/docs/platform/mcp) to give your agents the same view.

You now have an order to follow on your own repository: from the first `putnami observe` on a laptop, to a note on each pull request, to a verdict that holds each change to the specs you already keep.
