# Jira collaboration provider

`@putnami/jira-collaboration` implements the **tasks** collaboration contract
([`protocols/collaboration`](../../protocols/collaboration/README.md)) on Jira
Cloud: tasks are the issues of one Jira project. Jira holds no change
proposals, so the provider offers `tasks` alone.

## Use it

Declare the extension and bind the contract in `putnami.workspace.json`:

```json
{
  "extensions": ["/tooling/jira-collaboration"],
  "options": {
    "collaboration": {
      "tasks": {
        "provider": "@putnami/jira-collaboration",
        "version": 1,
        "settings": {
          "site": "acme.atlassian.net",
          "project": "ENG"
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
| `site` | required | The Jira Cloud host, such as `acme.atlassian.net`. The provider reaches `https://<site>/rest/api/2`. |
| `project` | required | The project key. `find` and `create` stay in this project; `get`, `update` and `transition` reach any issue of the site by id. |
| `issueType` | `Task` | The issue type `create` opens. |

An unknown setting is refused. Settings never carry a credential.

### Credentials

The provider reads `JIRA_EMAIL` and `JIRA_API_TOKEN`, an
[Atlassian API token](https://id.atlassian.com/manage-profile/security/api-tokens),
and sends them as HTTP Basic credentials to the configured site only. Without
them, every call answers `denied` (`jira.unauthenticated`) and sends nothing.
No answer, error or message carries the token: the runtime removes it, its
Basic form and every string shaped like an Atlassian API token from
everything it writes.

## What it offers

| Offered | Not offered |
|---|---|
| `find`, `get`, `create`, `update`, `transition` | `assign`, `link`, `claim` |

- **References.** A task is `{"source": "jira:<site>", "id": "<numeric issue
  id>"}`, and its `key` is `PROJ-123`. The id stays when a ticket moves to
  another project; the key does not.
- **States.** A status category decides the state: To Do is `open`, In
  Progress is `in_progress`, Done is `done`, and Done with the resolution
  "Won't Do" or "Duplicate" is `canceled`. `providerState` is the status name.
  No status is configured: a workflow's statuses already carry their category.
- **Transitions.** `transition` takes the first transition Jira offers whose
  target status is in the target category. `canceled` also sets the
  resolution "Won't Do", and needs a transition whose screen offers it. A task
  already in the state is left as it is. `blocked` has no category: a
  transition or a create to it is `unsupported`, and so is a state the
  workflow offers no transition into; the message lists the transitions Jira
  offered. `create` in another state than `open` opens the issue, then moves
  it.
- **Revisions.** A revision is `v` and the first 32 hex digits of the SHA-256
  of the summary, description, status name, status category, resolution,
  sorted labels and sorted assignee account ids. A comment does not change it.
  Jira has no conditional write: `update` and `transition` compare
  `expectedRevision` with the issue read just before the write, so a writer
  between the two is not detected.
- **Find.** The provider builds the JQL: the bound project, the status
  categories of the requested states, one `labels =` clause per label and the
  text matched against summary and description, ordered by key. Labels and
  text are quoted, and the characters JQL's text search reads as operators
  become spaces, so a caller's text never adds a clause. A cursor holds
  Jira's page token and the last key number read, so the caller may change
  the page size between pages. Jira's search index trails a write by seconds.

## Failures and retries

A read that got no definite answer — no connection, a broken one, a deadline,
or a 5xx — is sent up to 3 times. A definite 4xx answer, a rate limit
included, is never repeated. A write is sent exactly once.

| Operation | Identity that prevents a duplicate | After an answer is lost |
|---|---|---|
| `create` | The idempotency key, as the hidden label `putnami-idempotency-<sha256>` and the request digest in the issue property `putnami.idempotency` | Looks the key up again: found is `ok`, not found is `unresolved` |
| `update`, `transition` | The issue id | Reads the issue: the requested change in place is `ok`, otherwise `unresolved` |

The read-back is tried 3 times, 1 then 2 seconds apart. `unresolved` is never
retryable; its `reconcile` names the read, or the keyed repeat, that settles
it.

Before it creates, `create` looks for its key:

1. With JQL, among the issues the credential's account created that carry
   the label. The label's hash covers the project, so it names one binding.
2. By key, above the newest key neither created nor updated in the last 30
   minutes. Jira's search shows an issue seconds after its create, and not in
   key order; a read by key shows it at once. So the lookup lists the issues
   above that key, reads by key each key the list misses, then reads every
   key above the newest one until 3 keys in a row hold nothing. A repeat
   right after a create finds it.

The same request digest returns the issue (`created: false`); another digest
is a `conflict`. Labels are a set: their order does not change the request.
A lookup that needs more than 50 reads by key is refused, retryable
(`unavailable`, `idempotency.unverifiable`); deleted keys and keys hidden
from the account count as reads. A lookup in a project that created or
updated more than 500 issues in 30 minutes is refused, not retryable. The
label never appears in a task's `labels`, `update` keeps it, and a request
that names a label with its prefix is `invalid`. Two concurrent creates with one key are not
detected; sequential repeats are.

| Failure | Outcome |
|---|---|
| No credential | `denied`, `jira.unauthenticated` |
| 401 | `denied`, `jira.unauthorized` |
| 403 | `denied`, `jira.forbidden` |
| 429 | `unavailable`, retryable, `jira.rate_limited` |
| 404, or a reference from another source | `not_found` |
| 409, or a stale revision | `conflict` |
| 400 and other 4xx | `invalid`, `jira.rejected`, with Jira's field errors |
| Unreachable, or no definite answer to a read | `unavailable`, retryable |
| A task created, then its transition failed | `unresolved`, `jira.incomplete`: the message names the issue; read it and transition it |

## Layout

- `putnami.extension.json` — the runtime declaration and the `tasks`
  provider with its operations.
- `cmd/putnami-jira-collaboration` — the runtime: the `__putnami
  runtime-info` handshake and the `provider` entry point.
- `internal/jira` — the REST client: credential, endpoint, redaction and
  failure classification.
- `internal/provider` — the operation handlers.
- `bin/live-test` — the live proof against a real Jira project.

## Test it

`./putnamiw test --projects @putnami/jira-collaboration --enforce-coverage`
runs the shared contract scenarios of
[`providertest`](../../protocols/collaboration/providertest/providertest.go)
and the provider's own tests against an in-memory Jira stand-in: a lagging
search index, lost answers, outages, rate limits, workflows without a
transition and credential echoes.

The live proof runs the same scenarios against a real Jira Cloud project. It
opens, edits and moves issues there, and when it ends, passed or failed, it
deletes every issue the credential's account created in the project during
the run. It runs only on request, with `JIRA_EMAIL` and `JIRA_API_TOKEN` set,
and only in a project whose description carries
`putnami-collaboration-live-test`:

```bash
tooling/jira-collaboration/bin/live-test <site host> <project key>
```

The account needs the Delete Issues permission in that project.

## Decisions

- [ADR 0001 — Jira collaboration provider](doc/adr/0001-jira-collaboration-provider.md)
