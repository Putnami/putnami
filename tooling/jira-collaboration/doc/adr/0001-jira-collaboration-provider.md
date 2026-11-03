# ADR 0001 — Jira collaboration provider

- **Status**: accepted
- **Scope**: `@putnami/jira-collaboration` (`tooling/jira-collaboration`)

## Context

The collaboration contracts
([protocols ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md))
put task operations behind provider-neutral calls. A team that tracks its work
in Jira Cloud needs a `tasks` provider, so an agent finds, reads, creates,
updates and moves Jira tickets through `putnami tasks …` with the guarantees
the contract asks for: exact references, revisions, idempotent creation, and a
truthful answer when a write's outcome is unknown. Jira holds no change
proposals, so the provider implements `tasks` alone.

## Decision

1. **The provider speaks Jira Cloud REST API v2 itself.** Every operation is a
   Go `net/http` request, so the provider can tell a request that never left
   the process (`unavailable`) from one written without an answer
   (`unresolved`), and repeat only reads. Version 2 serves a description as a
   plain string, so `body` round-trips without converting Atlassian Document
   Format, the rich-text JSON of version 3. No dependency is added.

2. **Settings name the site and the project; the environment holds the
   credential.** `settings.site` is the bare host (`acme.atlassian.net`),
   `settings.project` the project key, `settings.issueType` the type `create`
   opens (`Task` unless set). The provider reads `JIRA_EMAIL` and
   `JIRA_API_TOKEN` and sends them as HTTP Basic credentials only to
   `https://<site>`. The only other destination is the loopback stand-in
   `PUTNAMI_JIRA_API_URL` names, for tests. The runtime removes the token, its
   Basic form and every string shaped like an Atlassian API token from
   everything it writes.

3. **A reference is the issue id, qualified by the site.** A task is
   `{"source": "jira:<site>", "id": "<numeric issue id>"}`; its `key` is
   `PROJ-123`. The id stays when a ticket moves to another project, and the key
   does not. `get`, `update` and `transition` therefore reach any issue of the
   site by id, while `find` and `create` stay in the bound project.

4. **States come from status categories.** To Do is `open`, In Progress is
   `in_progress`, Done is `done`, and Done with the resolution "Won't Do" or
   "Duplicate" is `canceled`; `providerState` keeps the status name. No status
   name is configured: a workflow's statuses already carry their category.
   `transition` takes the first transition Jira offers whose target status is
   in the target category, and sets the resolution "Won't Do" for `canceled`.
   Jira has no category for `blocked`: transition and create to it are
   `unsupported`, and `find` matches nothing for it.

5. **A revision covers what a task shows.** It is `v` and the first 32 hex
   digits of the SHA-256 of the JSON array of summary, description, status
   name, status category, resolution, sorted labels and sorted assignee account
   ids. Issue #796 listed the status alone; the category and the resolution
   join it because a done ticket that becomes canceled can keep its status,
   and the contract requires the revision to change whenever the state does.
   `updated` is not used: a comment moves it. Jira has no conditional write, so
   `update` and `transition` compare the revision with the issue read just
   before the write and declare `preconditions: checked`.

6. **The idempotency key is a hidden label; the request digest is an issue
   property.** A Jira description has no hidden text, so `create` adds the
   label `putnami-idempotency-<sha256 of the key>` and the issue property
   `putnami.idempotency` holding the digest of the request, both in the create
   call itself; the key's hash also covers the project, so two bindings of one
   site never share a key. A repeat looks the label up with JQL among the
   issues the credential's account created. Jira's search index trails a
   create by seconds, and not in key order, while a read by key does not. So
   the repeat then lists the issues of the project opened in the last 30
   minutes, reads by key each recent key that list misses, and reads every
   key above the newest one until three keys in a row hold nothing. The same digest returns the ticket (`created: false`), another
   digest is a conflict. The label never appears in a task's `labels`,
   `update` keeps it, and a request that names one is `invalid`.

7. **A write is sent once and read back when its outcome is unknown.** After a
   lost answer or a 5xx, `create` looks its key up again and `update` and
   `transition` read the issue again, up to three times. A write the reads
   cannot establish answers `unresolved`, names the read or the idempotent
   repeat that settles it, and is never sent again.

8. **`find` builds JQL and pages by key.** The query is the bound project, the
   status categories of the requested states, one `labels =` clause per label
   and the text matched against summary and description, ordered by key. Text
   and labels are quoted, and the characters JQL's text search reads as
   operators are replaced by spaces, so a caller cannot add a clause. The
   provider separates `done` from `canceled` itself, so JQL never names a
   resolution a site may lack. A cursor is Jira's page token and the last key
   number read: a request reads Jira's page again and skips what it returned,
   so the caller may change the page size between pages.

## Consequences

- A workspace binds `tasks` to this provider with a site and a project, and
  needs no other configuration for its workflow's statuses.
- Two concurrent creates with one key can each open a ticket. A lookup also
  misses a ticket created above three deleted, hidden or moved keys in a row
  that the search does not show yet, and one the search still does not show
  after 30 minutes.
- A keyed create in a project that opens more than 500 issues in 30 minutes,
  or that needs more than 50 reads by key, is refused rather than risk a
  duplicate.
- `assign`, `link` and `claim` are not offered: a Jira issue has one assignee
  and no exclusive lease the contract can use.
- `bin/live-test` runs the contract scenarios against a real Jira Cloud
  project with the owner's credentials; it is never part of the gate.
