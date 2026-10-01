# GitHub collaboration provider

`@putnami/github-collaboration` implements the **tasks** and **proposals**
collaboration contracts ([`protocols/collaboration`](../../protocols/collaboration/README.md))
on GitHub: tasks are the issues of one repository, proposals are its pull
requests. A workspace binds each contract on its own; binding one does not
bind the other.

## Use it

Declare the extension and bind the contracts in `putnami.workspace.json`.
This is Putnami's own configuration, given as a reference, not a default:

```json
{
  "extensions": ["/tooling/github-collaboration"],
  "options": {
    "collaboration": {
      "tasks": {
        "provider": "@putnami/github-collaboration",
        "version": 1,
        "require": ["assign"],
        "settings": {
          "repository": "Putnami/putnami",
          "states": {
            "open": ["status/confirmed", "status/audit-finding"],
            "in_progress": ["status/in-progress"],
            "blocked": ["status/needs-review", "status/needs-design"]
          },
          "stateLabelPrefix": "status/"
        }
      },
      "proposals": {
        "provider": "@putnami/github-collaboration",
        "version": 1,
        "settings": {
          "repository": "Putnami/putnami",
          "assignAuthor": true,
          "inheritLabelPrefixes": ["group/", "scope/", "area/", "severity/", "priority/"]
        }
      }
    }
  }
}
```

Then call `putnami tasks <operation>`, `putnami proposals <operation>` or the
MCP tools `tasks.*` and `proposals.*`
([CLI guide](../cli/doc/25-collaboration-providers.md)). The first call
compiles the runtime from this directory (`bin/prepare`).

### Settings

| Contract | Setting | Default | Meaning |
|---|---|---|---|
| both | `repository` | required | `owner/name` of the repository served. A request that names another repository is refused. |
| both | `host` | `github.com` | The GitHub host. Another host is GitHub Enterprise Server, reached at `https://<host>/api/v3`; a keyed `tasks create` needs a release whose GraphQL API serves repository discussions. |
| tasks | `states` | no labels | The labels of each semantic state: `open`, `in_progress`, `blocked`, `done`, `canceled`. The first label of a state is the one a transition applies; the others are recognized when reading. |
| tasks | `stateLabelPrefix` | none | Every label with this prefix is a state label too: a transition removes it, and a task's `labels` never show it. |
| tasks | `keyScanPages` | `3` | How many pages of 100 of the account's most recent issues a create reads for its idempotency key before it asks the search index (1 to 10). |
| proposals | `allowMerge` | `false` | Lets `proposals merge` merge. Without it, merge is refused before anything is sent. |
| proposals | `assignAuthor` | `false` | Assigns the credential's account to every proposal an upsert writes. |
| proposals | `inheritLabelPrefixes` | none | Copies onto every proposal an upsert writes the labels with these prefixes that the issues its body closes carry (`Closes #12`). |

An unknown setting is refused. Settings never carry a credential.

### Credentials

The provider finds a credential the way the `gh` CLI does, in this order:

1. `GH_TOKEN`, then `GITHUB_TOKEN`, for `github.com`.
2. `GH_ENTERPRISE_TOKEN`, then `GITHUB_ENTERPRISE_TOKEN`, for another host,
   and only when `GH_HOST` names that host.
3. `gh auth token --hostname <host>`: the token `gh auth login` stored.

Without one, every call answers `denied` (`github.unauthenticated`) and sends
nothing. The token is sent only to the configured host's API. No answer, error
or message carries it: the runtime removes it, and every string shaped like a
GitHub token, from everything it writes.

## What it offers

| Contract | Offered | Not offered |
|---|---|---|
| tasks v1 | `find`, `get`, `create`, `update`, `transition`, `assign`, `link` | `claim` |
| proposals v1 | `find`, `upsert`, `status`, `review`, `merge` (with `allowMerge`) | — |

- **States.** An open issue is `blocked`, `in_progress` or `open` by the first
  of those states whose label it carries, and `open` without one. A closed
  issue is `canceled` when GitHub closed it as not planned or as a duplicate,
  and `done` otherwise. A transition removes every state label and applies the
  target's first label; `done` and `canceled` also close the issue with the
  matching reason, and an open state reopens a closed one. A transition to the
  state an open issue already reads as still applies the first label when
  another label decided it (`status/needs-design` becomes
  `status/needs-review` in the reference mapping), and writes nothing when the
  first label decided it. An open issue without a state label reads as open,
  and a transition to `open` still gives it the first label of `open`
  (`status/confirmed` in the reference mapping); with no label for `open`, it
  writes nothing. A closed issue in the closed state asked for is left
  as it is. A transition to a state without a label, other than `open`, is
  `unsupported`. `create` applies the first label of `in_progress` or
  `blocked` when asked for them, and no state label for `open`, which an issue
  without one already reads as; `create` in `done` or `canceled` opens the
  issue, then closes it. `create` and `update` refuse a state label: only
  `transition` changes a state.
- **Assignment is information.** `assign` replaces the assignees (`@me` is the
  credential's account); GitHub drops a login it cannot assign, and the answer
  lists who is actually assigned. Nothing is claimed exclusively: GitHub
  cannot enforce it, so `claim` is not offered.
- **Hierarchy.** `link` sets or removes the parent through GitHub sub-issues;
  the parent must be an issue of the same repository. `get` and `link` report
  the parent; `find` does not. The parent is outside the revision (see below),
  and the `link` tool's description says so, which is how the tasks contract
  lets a provider leave it out.
- **Proposal labels and assignees.** Every proposal answer lists the pull
  request's `labels` and `assignees`, `[]` when it has none. An upsert answers
  after `assignAuthor` and `inheritLabelPrefixes` applied their additions, so
  its answer shows them, and a caller that reads the proposal back can check
  they are still there. The contributor finalizer does.
- **Checks.** `proposals status` reports the check runs and commit statuses of
  the head commit: `failing` when any fails, `pending` when any is not
  finished, `passing` when all passed or were skipped, `none` when there is
  none. At most 100 are listed, failing and pending first.
- **Reviews.** `status` lists submitted reviews; pending and dismissed ones are
  not reported. GitHub refuses an approval or a change request by the pull
  request's author: that is `invalid`.
- **Merge** needs `allowMerge` and an `expectedHeadCommit` that is still the
  head. Binding the provider authorizes nobody to merge.

### References, revisions and pages

- A reference is `{"source": "github:<owner>/<name>", "id": "<number>"}`,
  lowercase, with the host before the owner on another host
  (`github:ghe.example.com/acme/app`). A review is `review-<id>`. A source is
  compared case-insensitively, and results always carry the lowercase form. A
  reference from another source is `not_found`.
- A revision is a digest of what a task or proposal shows and a caller can
  change — title, body, state, labels and assignees; for a proposal also the
  draft flag, base, head and head commit. A comment does not move it, and
  neither does a task's parent: `find` does not read parents, and every
  operation reports one revision for one issue. So `link` leaves the revision
  as it is, and an `expectedRevision` does not detect a concurrent `link`.
- `expectedRevision` is compared with the item read just before the write.
  GitHub has no conditional write, so the provider declares
  `preconditions: checked`: a writer between the read and the write is not
  detected.
- Lists are read oldest first from GitHub's numbered pages of 100. The cursor
  records the last number a page considered, so an item opened, closed or
  deleted during a traversal moves no other item across a page boundary.
  `tasks find` reads GitHub's issue list, or its issue search when it has a
  `query`. Both trail recent writes: an issue appears in them seconds after
  its create was answered, and in the search sometimes minutes after. GitHub's
  search serves the first 1,000 matches of a query: a `query` traversal ends
  there, with no next page.
- A fork's head is `owner:branch`. `upsert` checks a `headCommit` of a fork's
  branch in the fork named like this repository: GitHub names no fork by its
  owner alone, so a fork renamed away from that name answers `not_found`
  (`head.missing`); upsert it without `headCommit`.

## Failures and retries

A read that got no definite answer — no connection, a broken one, a deadline,
or a 5xx — is sent up to 3 times. A definite 4xx answer, a rate limit
included, is never repeated. A write is sent exactly once; the provider never
repeats one itself.

| Operation | Identity that prevents a duplicate | After an answer is lost |
|---|---|---|
| `tasks create` | The idempotency key, recorded in the issue body as a hidden comment | Looks for the key among the newest issues, as below: found is `ok`, not found is `unresolved` |
| `tasks update`, `transition`, `assign` | The issue number | Reads the issue: the requested change in place is `ok`, otherwise `unresolved` |
| `tasks link` | The issue number | Reads the parent: the requested parent is `ok`, otherwise `unresolved`. A change GitHub acknowledged is `ok` even while the read does not show it yet |
| `proposals upsert` | The base and head: GitHub keeps one open pull request per pair | Finds the open pull request of the pair: found is `ok`, not found is `unresolved` |
| `proposals review` | The idempotency key, recorded in the review body as a hidden comment | Lists the pull request's reviews for the key: found is `ok`, not found is `unresolved` |
| `proposals merge` | The pull request number and expected head | Reads the pull request: merged is `ok`, otherwise `unresolved` |

The read-back is tried 3 times, 1 then 2 seconds apart. `unresolved` is never
retryable; its `reconcile` names the read, or the keyed repeat, that settles
it.

Before it creates, `tasks create` looks for its key:

1. In the newest 100 issues and pull requests of the repository.
2. In each issue that list may not show yet, read by number. GitHub's lists
   show an issue only seconds after its create was answered; a read by number
   shows it at once, so a repeat right after a create finds it. The lookup
   reads every number above the list's newest one until 3 numbers in a row
   are held by nothing. The issues endpoint answers 404 for a discussion as
   for a number not given yet, so each run of 404 answers is sent to GraphQL
   in one query that asks which of those numbers discussions hold: a
   discussion does not end the run, and a number counts as held by nothing
   only when GitHub says no discussion holds it or the repository takes no
   discussions. It reads every number missing below the newest one, down to
   the newest listed issue opened more than 30 minutes before GitHub's clock:
   an issue below that one would be listed. A deleted issue (410), an issue
   moved to another repository and a discussion are read like any other
   number and never answer for a key.
3. In `keyScanPages` pages of the account's newest issues, then in the search
   index, which matches the key as a word of the body (proven against GitHub
   by `bin/live-test`).

It refuses to create (`unavailable`, `idempotency.unverifiable`) when the
search or the discussion query cannot answer (retryable), when GitHub answers
the discussion query with an error other than "no discussion has this
number" (not retryable), and when step 2 needs more than 100 reads, a
discussion query counting as one. That last refusal is retryable when some of
the numbers read are issues GitHub does not list yet; when they are all
deleted, moved or unreadable, it is not, and its message counts them: a
repeat meets the same numbers until GitHub lists a newer issue or pull request
and 30 minutes pass after it was opened. In a repository that takes
discussions, the credential must read them: without that access a create is
`denied` (`idempotency.unverifiable`) and names it. A `gh` login and a classic
token with the `repo` scope read them; a fine-grained token needs the
Discussions read permission. Only
an issue or review the credential's account opened answers for a key: a copied
marker answers for nothing. Finding that account reads `/user`, so
`tasks create`, `proposals review`, `@me` and `assignAuthor` need a user
credential (a `gh` login or a personal access token), not a GitHub App
installation token. Two concurrent creates with one key are not detected;
sequential repeats are.

| Failure | Outcome |
|---|---|
| No credential | `denied`, `github.unauthenticated` |
| 401 | `denied`, `github.unauthorized` |
| 403 | `denied`, `github.forbidden` |
| 403 or 429 reporting a rate limit | `unavailable`, retryable, `github.rate_limited` |
| 404, 410, or a reference from another source | `not_found` |
| 409, 412, a stale revision, a moved head | `conflict` |
| 422 and other 4xx | `invalid`, `github.rejected` |
| Unreachable, or no definite answer to a read | `unavailable`, retryable |
| A proposal written, then its assignment or labels failed | `unresolved`, `github.incomplete`: repeating the upsert applies the rest; after a refusal, every repeat is refused the same way until the credential may do it or the setting is off |
| A task created in `done` or `canceled`, then its close failed | `unresolved`, `github.incomplete`: the message names the open issue; read it and transition it |

## Layout

- `putnami.extension.json` — the runtime declaration and one tool per
  operation, each marked `putnami.dev/provider`.
- `cmd/putnami-github-collaboration` — the runtime: the `__putnami
  runtime-info` handshake and the `provider-tool` bridge.
- `internal/github` — the REST and GraphQL client: credentials, endpoints,
  and failure classification.
- `internal/provider` — the operation handlers.
- `bin/live-test` — the live proof against a real repository.

## Test it

`./putnamiw test --projects @putnami/github-collaboration --enforce-coverage`
runs the shared contract scenarios of
[`providertest`](../../protocols/collaboration/providertest/providertest.go)
and the provider's own tests against an in-memory GitHub stand-in: lost
answers, outages, rate limits, pagination, forks and credential echoes.

The live proof runs the same scenarios against a real repository, then waits
until GitHub's issue search finds an idempotency key the way a create asks for
it. It opens issues, branches, commits and pull requests there. When it ends,
passed or failed, it closes what the run opened — found by number, and only
what a scenario reported, a pull request on one of the run's branches, or an
issue carrying a scenario's label — and deletes the branches it created. It
names, and leaves open, anything else the account opened meanwhile. It runs
only on request and only in a repository that carries the topic
`putnami-collaboration-live-test`:

```bash
tooling/github-collaboration/bin/live-test <owner>/<name>
```

The REST API cannot delete issues, pull requests or commits: each run leaves
about 14 closed items and 10 commits on deleted branches. Recreate the test
repository when it grows large.

## Decisions

- [ADR 0001 — GitHub collaboration provider](doc/adr/0001-github-collaboration-provider.md)
