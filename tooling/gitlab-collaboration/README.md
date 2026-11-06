# GitLab collaboration provider

`@putnami/gitlab-collaboration` implements the **tasks** and **proposals**
collaboration contracts
([`protocols/collaboration`](../../protocols/collaboration/README.md)) on
GitLab: tasks are the issues of one project, on GitLab.com or a self-managed
instance, and proposals are its merge requests, so a GitLab-hosted repository
runs `build` and `change finalize` as a GitHub-hosted one does.

## Use it

Declare the extension and bind the contracts in `putnami.workspace.json`. The
`states` mapping and the proposal settings read like the GitHub provider's, so
a workspace that moves between the two keeps them:

```json
{
  "extensions": ["/tooling/gitlab-collaboration"],
  "options": {
    "collaboration": {
      "tasks": {
        "provider": "@putnami/gitlab-collaboration",
        "version": 1,
        "settings": {
          "project": "acme/platform/app",
          "host": "gitlab.example.com",
          "states": {
            "open": ["status::ready"],
            "in_progress": ["status::in-progress"],
            "blocked": ["status::blocked"],
            "canceled": ["status::canceled"]
          },
          "stateLabelPrefix": "status::"
        }
      },
      "proposals": {
        "provider": "@putnami/gitlab-collaboration",
        "version": 1,
        "settings": {
          "project": "acme/platform/app",
          "host": "gitlab.example.com",
          "assignAuthor": true,
          "inheritLabelPrefixes": ["type::"]
        }
      }
    }
  }
}
```

Then call `putnami tasks <operation>` and `putnami proposals <operation>`
([CLI guide](../cli/doc/25-collaboration-providers.md)). The first call
compiles the runtime from this directory (`bin/prepare`).

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `project` | required | The project's full path, `namespace/name` with any subgroups. |
| `host` | `gitlab.com` | The GitLab instance, reached at `https://<host>/api/v4`. |
| `states` | no labels | The labels of each semantic state: `open`, `in_progress`, `blocked`, `done`, `canceled`. The first label of a state is the one a transition applies; the others are recognized when reading. |
| `stateLabelPrefix` | none | Every label with this prefix is a state label too: a transition removes it, and a task's `labels` never show it. GitLab's scoped labels (`status::`) fit it. |
| `keyScanPages` | `3` | How many pages of 100 of the account's newest issues a create reads for its idempotency key before it asks GitLab's search (1 to 10). |

A proposals binding takes `project` and `host`, and:

| Setting | Default | Meaning |
|---|---|---|
| `allowMerge` | `false` | Lets `merge` merge. Unset, `merge` is `denied` before anything is sent: binding the provider authorizes nobody to merge. |
| `removeSourceBranch` | `false` | Asks GitLab to delete the source branch in the merge call itself. Unset, the merge request's own choice applies. |
| `assignAuthor` | `false` | Adds the credential's account to the assignees of every merge request an upsert writes. |
| `inheritLabelPrefixes` | none | Copies, onto every merge request an upsert writes, the labels with one of these prefixes that the issues its description names carry. |

An unknown setting is refused. Settings never carry a credential.

### Credentials

The provider finds one credential per instance, the way `glab` does:

1. `GITLAB_TOKEN`, for the host `GITLAB_HOST` names (a host or an `https`
   URL), and for `gitlab.com` when `GITLAB_HOST` is unset.
2. `glab config get token --host <host>`: the token `glab auth login`
   stored. A token `glab` keeps in an operating-system keyring is not read.
3. Inside a GitLab CI/CD job (`GITLAB_CI=true`) whose `CI_SERVER_HOST` or
   `CI_SERVER_FQDN` is the bound host, `CI_JOB_TOKEN`. GitLab lets a job
   token list and read issues and nothing more: with it, `find` and `get`
   work and every write answers `denied`. A pipeline that writes tasks or
   proposals sets `GITLAB_TOKEN`.

A job token is sent in the `JOB-TOKEN` header, any other token as
`Authorization: Bearer`. Without a credential, every call answers `denied`
(`gitlab.unauthenticated`) and sends nothing. No answer, error or message
carries a token: the runtime removes the one it resolved, and every string
shaped like a GitLab token (`glpat-`, `glcbt-` and the other published
prefixes), from everything it writes. A 401 or 403 says where the token came
from (`GITLAB_TOKEN`, `glab config get token` or `CI_JOB_TOKEN`), never the
token.

### Environment

`PUTNAMI_GITLAB_API_URL` points the client at a loopback `http` or `https`
stand-in for the GitLab API, and nothing else: any other URL is refused, so a
credential never leaves its own instance or this machine.

## What it offers

| Contract | Offered | Not offered |
|---|---|---|
| `tasks` | `find`, `get`, `create`, `update`, `transition`, `assign` | `link`, `claim` |
| `proposals` | `find`, `upsert`, `status`, `review`, `comments`, `merge` | |

### Tasks

- **References.** A task is `{"source": "gitlab:<host>/<project path>", "id":
  "<iid>"}`, lowercase, and its `key` is `#<iid>`. A reference from another
  source, or in another casing, is `not_found`.
- **States.** An opened issue is `blocked`, `in_progress` or `open` by the
  first of those states whose label it carries, and `open` without one. A
  closed issue is `canceled` when it carries a `canceled` label or GitLab
  closed it as a duplicate, and `done` otherwise. `providerState` is the
  deciding label, or `opened`, `closed` or `closed:duplicate`.
- **Transitions.** A transition removes every state label, applies the
  target's first label, and closes the issue for `done` and `canceled` or
  reopens it otherwise, in one request. `in_progress`, `blocked` and
  `canceled` need a label; without one they are `unsupported`. A task
  already in the state is left as it is, except an opened issue that reads as
  the state through another of its labels: it gets the first label. `create`
  applies the first label of `in_progress` or `blocked`, and opens then
  closes an issue asked for in `done` or `canceled`. `create` and `update`
  refuse a state label, and a label with a comma, which GitLab reads as a
  separator. A transition to `done` of an issue GitLab closed as a duplicate
  reopens it, which clears the duplicate link, then closes it.
- **Bodies are text.** GitLab runs a quick action (`/close`, `/label`,
  `/move`) from a description line that starts with a slash. `create` and
  `update` put a zero-width space before such a slash, and before a
  zero-width space that starts a line, so GitLab stores the line and runs
  nothing; a read removes exactly one: a task's body is what the caller
  wrote. Fenced code is escaped too, since GitLab versions disagree on what a
  fence is: a command copied from GitLab's page carries the invisible
  character.
- **Assignment is information.** `assign` replaces the assignees, named by
  username; `@me` is the credential's account. A username GitLab does not
  know is `invalid` and nothing is written. GitLab keeps only one assignee on
  a tier without multiple assignees, and the answer lists who is actually
  assigned. Nothing is claimed exclusively.
- **Hierarchy.** GitLab's issue hierarchy is an epic or a work item parent,
  outside the issues REST API, so `link` is not offered.
- **Revisions.** A revision is `v` and the first 32 hex digits of the SHA-256
  of title, description, state, duplicate link, sorted labels and sorted
  assignee usernames. A comment does not change it. GitLab has no
  conditional write: `update`, `transition` and `assign` compare
  `expectedRevision` with the issue read just before the write, so a writer
  between the two is not detected.
- **Find.** GitLab filters the project's issues on their state, every listed
  label and the text in title and description, oldest first; the provider
  then keeps the requested semantic states. A label named `None` or `Any`,
  keywords of GitLab's filter, is matched by the provider. Each read asks
  for the issues created at or after the millisecond of the last issue
  considered, and the cursor holds that millisecond with the ids already
  seen at it, or their count past 48, which skips by position only issues of
  that millisecond; once that tie is used up, the next read starts at the
  following millisecond. So a traversal always ends and lists
  every issue present throughout it once, whatever the page sizes, ties in
  creation time and issues closed meanwhile; only inside a tie of more than
  48 issues can closing some of them hide others.

### Proposals

- **References.** A proposal is `{"source": "gitlab:<host>/<project path>",
  "id": "!<iid>"}`, GitLab's own merge request reference: merge requests and
  issues number apart, so neither contract reads the other's reference. Its
  `change.repository` is the project path the settings name.
- **Exact identity.** A proposal is the merge request whose source branch, in
  the bound project, and target branch are the request's exact head and base.
  A merge request from a fork's branch of the same name is another one and is
  never answered, by identity or by reference. A head written `owner:branch` names a fork and is
  `invalid`, as is a repository other than the bound project; nothing is
  sent. GitLab keeps one open merge request per source and target branch, so
  `upsert` updates it, or opens it, and a repeated, concurrent or lost
  `upsert` never opens a second. `find` lists every merge request of the
  identity by iid; its cursor is `m<iid>`.
- **Head commit.** A requested `headCommit` must be the commit the branch
  points at, or the call is a `conflict` (`head.moved`) before anything is
  written. GitLab moves a merge request's head some time after a push: a
  `find` or `upsert` naming the commit the branch already points at re-reads
  the merge request for about 10 seconds, then answers `unavailable`,
  retryable (`head.lagging`).
- **Drafts.** GitLab marks a draft by its title prefix. `upsert` writes
  `Draft: ` before a draft's title and leaves it off a ready one; a
  proposal's `title` never shows the prefix, and an `upsert` without `draft`
  keeps the draft state. A requested title that starts with `Draft:`,
  `[Draft]`, `(Draft)`, `Draft -` or is `Draft` would set the state behind
  `draft`: it is `invalid` (`title.draft`) and nothing is written.
- **Bodies are text.** Descriptions and review bodies are escaped as task
  bodies are, so GitLab runs no quick action (`/merge`, `/approve`) from
  them, and a description reads back byte for byte.
- **Reviews are notes.** `review` posts one note holding the body, a hidden
  verdict and commit, and the idempotency marker. `approve` first approves
  the merge request at the reviewed head, which GitLab refuses once the head
  moved or, under the project's rules, to the merge request's author;
  `request_changes` first withdraws the account's approval, and GitLab blocks
  nothing on its note. A repeated key returns the note (`created: false`),
  other content under it is a `conflict`, and only a note the account wrote
  answers for a key. `status` lists every review note, with the verdict it
  records, then every current approval as an `approve` review
  (`approval-<iid>-<user id>`).
- **Checks.** `status` reads the commit statuses GitLab keeps for the head
  commit, pipeline jobs and external statuses alike, by name. GitLab lists
  the statuses of every pipeline of the commit, so the latest of each name,
  by id, decides. `success` passes, `failed` and `canceled` fail unless the
  job may fail, `skipped` and a `manual` job allowed to fail are skipped, and
  every other status, a blocking manual job included, is pending.
- **Comments.** `comments` lists every note people wrote, never a system
  note, by ascending note id; its cursor is `n<id>`. A review note lists its
  body without its hidden comments and names itself as its review; a diff
  note keeps its path, and its line when it is anchored to a line of the new
  file, at the head commit it was written on.
- **Publication additions.** `assignAuthor` and `inheritLabelPrefixes` only
  add. On a tier that keeps one assignee, the present assignee stays and the
  answer lists who is assigned. A scoped label (`key::value`) is exclusive,
  so of each key one label is added, the first the description names, and
  none when the merge request already carries that key. The issues a description names are read in GitLab's references, after
  a keyword (`Closes`, `Fixes`, `Resolves`, `Implements`, `Refs`,
  `Related to`): `#<iid>` in the bound project, `project#<iid>` in a project
  of its namespace, `group/project#<iid>`, and an `https` issue URL on the
  bound host.
  `!<iid>` names a merge request and adds nothing, and a URL on another host is
  never read.
- **Merge.** `merge` merges only at the expected head, which it sends as
  GitLab's `sha`, so a head that moves meanwhile is a `conflict`, as is a
  merge request GitLab cannot merge. `squash` asks GitLab to squash and is
  `unsupported` where the project never squashes; `merge` is served only
  where the project's merge method creates a merge commit and `rebase` only
  where it fast-forwards, both `unsupported` where the project always
  squashes; nothing is sent then. GitLab squashes by the merge request's own
  squash flag, which the project's default sets on create, so a named method
  first sets that flag; when the project or a protected branch's rule still
  decides otherwise, `merge` is `unsupported`. Every answer that merges
  nothing leaves the flag as it was: a rule the merge request's read already
  shows sends nothing, and a flag set for the merge, by the provider or by
  GitLab's own merge call once it accepted the credential and the head, is
  put back. The project's `never` and `always`
  refuse on the project's setting alone, even where a protected branch's
  rule would relax them: merge without a method there. No method leaves the choice to the project.
- **Revisions.** A proposal's revision covers the stored title, description,
  state, draft flag, base, head, head commit, labels and assignees; a note
  does not move it.

## Failures and retries

A read that got no definite answer — no connection, a broken one, a deadline,
or a 5xx — is sent up to 3 times. A definite 4xx answer, a rate limit
included, is never repeated. A write is sent exactly once.

| Operation | Identity that prevents a duplicate | After an answer is lost |
|---|---|---|
| `create` | The idempotency key, recorded at the end of the description as a hidden comment | Looks for the key among the account's newest issues: found is `ok`, not found is `unresolved` |
| `update`, `transition`, `assign` | The issue iid | Reads the issue: the requested change in place is `ok`, otherwise `unresolved` |
| `upsert` | The source and target branch: GitLab keeps one open merge request per pair | Reads the open merge request of the pair: it is `ok`, `created` only when it carries what was sent; none is `unresolved` |
| `review` | The idempotency key, recorded at the end of the note as a hidden comment | Looks for the key among the account's notes on the merge request: found is `ok`, not found is `unresolved` |
| `merge` | The expected head commit | Reads the merge request: merged is `ok`, otherwise `unresolved` |

The read-back is tried 3 times, 1 then 2 seconds apart. `unresolved` is never
retryable; its `reconcile` names the read, or the keyed repeat, that settles
it.

Before it creates, `create` looks for its key among the newest issues the
credential's account opened in the project, `keyScanPages` pages of 100, then
in GitLab's issue search, which finds the first half of the key's hash: each
hash is written as two words of 32 hex digits, since GitLab's index drops
longer words. GitLab's lists read its database, so a repeat right
after a create finds the issue. The same request returns the issue
(`created: false`); other content is a `conflict`. Labels are a set: their
order does not change the request. Only an issue the account opened answers
for a key: a copied marker answers for nothing. Two concurrent creates with
one key are not detected; sequential repeats are.

| Failure | Outcome |
|---|---|
| No credential | `denied`, `gitlab.unauthenticated` |
| 401 | `denied`, `gitlab.unauthorized` |
| 403 | `denied`, `gitlab.forbidden` |
| 429 | `unavailable`, retryable, `gitlab.rate_limited` |
| 404, a project the credential cannot see, or a reference from another source | `not_found` |
| 409, or a stale revision | `conflict` |
| 400, 422 and other 4xx | `invalid`, `gitlab.rejected`, with GitLab's message |
| Unreachable, or no definite answer to a read | `unavailable`, retryable |
| A task created in `done` or `canceled`, then its close failed | `unresolved`, `gitlab.incomplete`: the message names the open issue; read it and transition it |
| A merge request written, then `assignAuthor` or `inheritLabelPrefixes` failed | `unresolved`, `gitlab.incomplete`: the message names the merge request; repeat the upsert |
| An approval given or withdrawn, then the review note failed | `unresolved`, `gitlab.incomplete`: repeat the review with the same key |
| `merge` without `allowMerge` | `denied`, `merge.disabled`, nothing sent |
| A merge request with more than 2,000 notes, or more than 500 for one base and head | `unavailable`, `gitlab.unbounded` |
| A head commit with more than 1,000 commit statuses | The newest 1,000 decide; the checks' `detail` says the oldest were not read |

## Layout

- `putnami.extension.json` — the runtime declaration and the `tasks` and
  `proposals` providers with their operations.
- `cmd/putnami-gitlab-collaboration` — the runtime: the `__putnami
  runtime-info` handshake and the `provider` entry point.
- `internal/gitlab` — the REST client: credential discovery, endpoint,
  redaction and failure classification.
- `internal/provider` — the operation handlers.
- `bin/live-test` — the live proof against a real GitLab project.

## Test it

`./putnamiw test --projects @putnami/gitlab-collaboration --enforce-coverage`
runs the shared contract scenarios of
[`providertest`](../../protocols/collaboration/providertest/providertest.go)
and the provider's own tests against an in-memory GitLab stand-in: lost
answers, outages, rate limits, pagination, imported issues, merge requests,
notes, approvals, commit statuses and credential echoes.

The live proof runs the same scenarios against a real GitLab project. It
opens, edits and closes issues there, and creates branches one commit ahead of
the default branch to open and review merge requests from. When it ends,
passed or failed, it deletes every issue the credential's account opened in
the project during the run, or closes it when the account may not delete
issues, closes the merge requests from its branches and deletes the branches. It runs only on
request, and only in a project whose description carries
`putnami-collaboration-live-test`:

```bash
tooling/gitlab-collaboration/bin/live-test <project path> [host]
```

## Support

This package is `preview` in [`putnami.support.json`](../../putnami.support.json):
it can break in a minor release, and the release notes then state the change
you must make. For `stable`, it needs a consumer in this repository, a page on
the site, and a version gate that fails a release on an undeclared break.

## Decisions

- [ADR 0001 — GitLab collaboration provider](doc/adr/0001-gitlab-collaboration-provider.md)
- [ADR 0002 — Proposals on GitLab merge requests](doc/adr/0002-proposals-on-merge-requests.md)
