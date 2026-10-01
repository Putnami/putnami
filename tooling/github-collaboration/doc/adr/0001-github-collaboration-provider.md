# ADR 0001 — GitHub collaboration provider

- **Status**: accepted
- **Scope**: `@putnami/github-collaboration` (`tooling/github-collaboration`)

## Context

The collaboration contracts
([protocols ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md))
put task and proposal operations behind provider-neutral calls. GitHub needs a
provider that keeps what the contributor workflows rely on (one pull request per
branch, labels that carry issue status, assignment and label copying on
publication) and adds what the contracts require: exact identities, revisions,
idempotent creation, and a truthful answer when a write's outcome is unknown.

## Decision

1. **One extension implements tasks and proposals.** It is a provider package
   beside `@putnami/local-collaboration`, not part of the contributor
   workflows. A workspace binds each contract to it separately, with its own
   settings.

2. **The provider speaks the REST API itself; `gh` is only a credential store.**
   Every operation is a Go `net/http` request, so the provider can tell a
   request that never left the process (`unavailable`) from one written without
   an answer (`unresolved`), follow `Link` pagination, and repeat only reads.
   The credential is found as `gh` finds it: `GH_TOKEN`, then `GITHUB_TOKEN` for
   `github.com`; `GH_ENTERPRISE_TOKEN`, then `GITHUB_ENTERPRISE_TOKEN` only for
   the host `GH_HOST` names; otherwise `gh auth token --hostname`. Changing a
   pull request's draft flag and the discussion lookup of decision 8 use
   GraphQL, because REST cannot. No dependency is added.

3. **A credential goes only to its own host and never into an answer.** The API
   root is derived from `settings.host`; settings cannot name a URL. The only
   override is an environment variable restricted to loopback. A token must
   consist of letters, digits, `_`, `.` and `-`, so it cannot inject a header.
   The runtime removes it, and every string shaped like a GitHub token, from
   everything it writes, because the orchestrator cannot redact a token read
   from the `gh` keyring.

4. **References are source-qualified by repository.** A task or proposal is
   `{"source": "github:<owner>/<name>", "id": "<number>"}`, lowercase, prefixed
   by the host on another host; a review is `review-<id>`. A binding serves one
   repository: a request naming another is `invalid`, and a reference another
   source issued is `not_found`. An answer echoes the repository and head
   spelling of the request. A fork head `owner:branch` has its `headCommit`
   checked in the fork named like the served repository; a renamed fork answers
   `not_found` and is upserted without `headCommit`.

5. **Task states map to labels through settings.** `settings.states` lists the
   labels of each semantic state: the first is applied by a transition, the
   others are recognized. `settings.stateLabelPrefix` makes every label with the
   prefix a state label. `done` and `canceled` are closed issues (reason
   completed or not planned; a duplicate reads as canceled). An open issue with
   labels of several states reads, in order, as blocked, in progress, then
   open. A state that needs a label and has none is `unsupported`.
   - State labels change only through `transition`: `create` and `update`
     refuse them, and a task's `labels` never show them.
   - `create` applies a state label only for `in_progress` and `blocked`. An
     issue created `open` carries none, so an agent never applies a label a
     repository's triage may reserve for people.
   - A transition always leaves the target state's first label on the issue,
     even when the issue already reads as that state: an issue blocked through
     `status/needs-design` still moves to `status/needs-review`, and an
     unlabeled open issue transitioned to `open` gets `open`'s first label. A
     transition records a decision, and treating an unlabeled issue as settled
     would make it a silent no-op on exactly the issues triage has not seen. One
     postcondition also gives one read-back after a lost answer. A binding whose
     `open` has no label writes nothing.

   Putnami's mapping is a reference configuration in the README, not a default:
   `status/needs-review` and `status/needs-design` read as blocked, and a
   transition to blocked applies `status/needs-review`.

6. **A revision digests the visible, changeable content**: title, whole body,
   state and reason, labels and assignees; for a pull request also the draft
   flag, base, head and head commit. `updated_at` is not used: a comment would
   move it, and two writes in one second would share it. A task's parent is
   outside the revision, because GitHub serves it on a separate read `find`
   does not make. So `link` does not move the revision and `expectedRevision`
   does not detect a concurrent `link`; the `link` tool's description declares
   this exception
   ([collaboration ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)).

7. **Preconditions are `checked`, never `atomic`.** GitHub has no conditional
   write for issues or pull requests. The provider compares `expectedRevision`
   with a read made just before the write. It offers no `claim`: GitHub cannot
   hold an issue exclusively.

8. **Idempotency is recorded in the item it produced.** `tasks.create` and
   `proposals.review` append a hidden comment to the body with a digest of the
   operation and key and a digest of the request. An update keeps it; reads
   never show it. Only an issue or review the credential's account opened
   answers for a key, so a copied marker cannot return someone else's item;
   finding the account reads `/user`, which needs a user credential, not an App
   installation token. A review looks through every review of its pull request.
   `proposals.upsert` needs no key: GitHub refuses a second open pull request
   for one base and head, and the provider treats the refusal as "found".

   GitHub lists and searches a new issue only seconds after its create, while a
   read by number shows it at once. A keyed create therefore looks for its key
   in the newest page of issues, then by number in every issue that page may
   not show yet, then in the account's newest issues, then in the search index.
   - Reads by number cover each number above the newest listed issue until 3 in
     a row are held by nothing, and each missing number below it down to the
     newest issue opened more than 30 minutes before GitHub's `Date`.
   - Issues, pull requests and discussions share one number sequence, and the
     issues endpoint answers 404 for both a discussion and an unused number. A
     run of 404s above the newest is sent to GraphQL in one query: a discussion
     resets the run, and a number is held by nothing only when GitHub answers
     `NOT_FOUND` for it or says the repository has no discussions. Any other
     answer (`FORBIDDEN`, an unknown error, no answer) refuses the create.
   - The lookup assumes GitHub lists an issue within 30 minutes, that a read by
     number shows an item as soon as it exists, and that a number hidden from
     the credential is not the one a lost create took. A deleted (410) or moved
     issue costs one read and stops being read 30 minutes after a newer issue
     is listed.
   - The create is refused rather than risk a duplicate when search cannot
     answer, GitHub does not say whether a discussion holds a number, or more
     than 100 reads are needed (a discussion query counts as one). The refusal
     is retryable when GitHub did not answer or an issue is not listed yet;
     otherwise it names the blocker: the count of deleted, moved and unreadable
     numbers, the missing discussion access (`denied`), or GitHub's answer.

   Two concurrent calls with one key are not detected; sequential repeats are.

9. **A write is sent once; an unknown outcome is read back.** A write whose
   headers were sent and that got no definite answer, or got 5xx, is uncertain.
   The provider reads the item back up to 3 times and answers from what it
   finds, otherwise `unresolved` with the read that settles it. It never
   repeats a write, and never replays one at a redirect's location. A read with
   no definite answer is sent up to 3 times; a definite 4xx, rate limits
   included, never again. An operation whose first write is known to have
   happened says so: a create in `done` or `canceled` whose closing write fails
   is `unresolved` (`github.incomplete`) naming the open issue, and a `link`
   GitHub acknowledged answers `ok` with that parent even before a read shows
   it.

10. **Publication additions are proposals-binding settings.** `assignAuthor`
    assigns the credential's account; `inheritLabelPrefixes` copies the
    prefixed labels of the issues the body closes. Both only add. When either
    fails after the pull request was written, the upsert is `unresolved`
    (`github.incomplete`) and a repeat finishes it, except after a refusal,
    which the answer says every repeat will meet until the credential may act
    or the setting is off.

11. **Merge is optional and off.** The tool exists so capability discovery is
    truthful, but it refuses before sending anything unless
    `settings.allowMerge` is true, and merges only the expected head commit.

12. **The contract scenarios are shared; the live proof is opt-in.** This
    provider runs the `go.putnami.dev/protocol/collaboration/providertest`
    scenarios against an in-memory GitHub stand-in in its ordinary tests. They
    run against a real repository only through `bin/live-test`, and only in a
    repository with the topic `putnami-collaboration-live-test`. Cleanup closes
    only what the run opened (a reported item, a pull request on a run branch,
    an issue with a scenario label) and names anything else the account opened
    meanwhile.

## Consequences

- Skills bind GitHub without knowing its labels, commands or identifiers; a
  repository with other status labels changes settings, not skills.
- The provider needs network access and a credential; without one every call is
  `denied` and nothing is sent.
- A `find` can miss an issue opened seconds earlier; a keyed create cannot. A
  `find` with a `query` ends at GitHub search's 1,000 matches.
- A keyed create costs one list request, 3 reads above the newest issue, one
  GraphQL query per run of 404s, and one read per recent gap. More than 100
  deleted or moved issues newer than every listed one block creates until a
  newer issue is listed and 30 minutes pass.
- A keyed create needs a GraphQL schema with repository discussions; a GitHub
  Enterprise Server release without them refuses every keyed create.
- A concurrent writer between a read and a write is not detected.
