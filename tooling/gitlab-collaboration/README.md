# GitLab collaboration provider

`@putnami/gitlab-collaboration` implements the **tasks** collaboration
contract ([`protocols/collaboration`](../../protocols/collaboration/README.md))
on GitLab: tasks are the issues of one project, on GitLab.com or a
self-managed instance. The **proposals** contract over merge requests is not
served yet.

## Use it

Declare the extension and bind the contract in `putnami.workspace.json`. The
`states` mapping reads like the GitHub provider's, so a workspace that moves
between the two keeps it:

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
      }
    }
  }
}
```

Then call `putnami tasks <operation>`
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

An unknown setting is refused. Settings never carry a credential.

### Credentials

The provider finds one credential per instance, the way `glab` does:

1. `GITLAB_TOKEN`, for the host `GITLAB_HOST` names (a host or an `https`
   URL), and for `gitlab.com` when `GITLAB_HOST` is unset.
2. `glab config get token --host <host>`: the token `glab auth login`
   stored. A token `glab` keeps in an operating-system keyring is not read.
3. Inside a GitLab CI/CD job (`GITLAB_CI=true`) whose `CI_SERVER_HOST` or
   `CI_SERVER_FQDN` is the bound host, `CI_JOB_TOKEN`. GitLab decides what a
   job token may reach; a refusal answers `denied`.

A job token is sent in the `JOB-TOKEN` header, any other token as
`Authorization: Bearer`. Without a credential, every call answers `denied`
(`gitlab.unauthenticated`) and sends nothing. No answer, error or message
carries a token: the runtime removes the one it resolved, and every string
shaped like a GitLab token (`glpat-`, `glcbt-` and the other published
prefixes), from everything it writes.

### Environment

`PUTNAMI_GITLAB_API_URL` points the client at a loopback `http` or `https`
stand-in for the GitLab API, and nothing else: any other URL is refused, so a
credential never leaves its own instance or this machine.

## What it offers

| Offered | Not offered |
|---|---|
| `find`, `get`, `create`, `update`, `transition`, `assign` | `link`, `claim` |

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
  separator.
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
  then keeps the requested semantic states. A cursor names GitLab's page and
  the creation time and id of the last issue considered, so an issue opened
  or deleted during a traversal moves no other issue across a page boundary,
  and the page size may change between pages.

## Failures and retries

A read that got no definite answer — no connection, a broken one, a deadline,
or a 5xx — is sent up to 3 times. A definite 4xx answer, a rate limit
included, is never repeated. A write is sent exactly once.

| Operation | Identity that prevents a duplicate | After an answer is lost |
|---|---|---|
| `create` | The idempotency key, recorded at the end of the description as a hidden comment | Looks for the key among the account's newest issues: found is `ok`, not found is `unresolved` |
| `update`, `transition`, `assign` | The issue iid | Reads the issue: the requested change in place is `ok`, otherwise `unresolved` |

The read-back is tried 3 times, 1 then 2 seconds apart. `unresolved` is never
retryable; its `reconcile` names the read, or the keyed repeat, that settles
it.

Before it creates, `create` looks for its key among the newest issues the
credential's account opened in the project, `keyScanPages` pages of 100, then
in GitLab's issue search. GitLab's lists read its database, so a repeat right
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

## Layout

- `putnami.extension.json` — the runtime declaration and the `tasks`
  provider with its operations.
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
answers, outages, rate limits, pagination, imported issues and credential
echoes.

The live proof runs the same scenarios against a real GitLab project. It
opens, edits and closes issues there, and when it ends, passed or failed, it
deletes every issue the credential's account opened in the project during the
run, or closes it when the account may not delete issues. It runs only on
request, and only in a project whose description carries
`putnami-collaboration-live-test`:

```bash
tooling/gitlab-collaboration/bin/live-test <project path> [host]
```

## Decisions

- [ADR 0001 — GitLab collaboration provider](doc/adr/0001-gitlab-collaboration-provider.md)
