# ADR 0001 — GitLab collaboration provider

- **Status**: accepted
- **Scope**: `@putnami/gitlab-collaboration` (`tooling/gitlab-collaboration`)

## Context

The collaboration contracts
([protocols ADR 0001](../../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md))
put task and proposal operations behind provider-neutral calls. Today only
`@putnami/github-collaboration` serves a code host, so a repository hosted on
GitLab cannot run the contribution loop. This decision covers the first half
of a GitLab provider: the `tasks` contract over the issues of one GitLab
project, on GitLab.com or a self-managed instance. The `proposals` contract
over merge requests is a separate change of the same extension.

GitLab issues look like GitHub issues: a project-scoped number (`iid`), a
title, a description, labels, assignees, and two states, `opened` and
`closed`. So the provider follows the GitHub provider's decisions
([GitHub ADR 0001](../../../github-collaboration/doc/adr/0001-github-collaboration-provider.md))
wherever GitLab allows it, and departs from them only where GitLab differs.

## Decision

1. **The provider speaks the GitLab REST API v4 itself.** Every operation is a
   Go `net/http` request to `https://<host>/api/v4`, so the provider can tell
   a request that never left the process (`unavailable`) from one written
   without an answer (`unresolved`), and repeat only reads. No dependency is
   added. The only other destination is the loopback stand-in
   `PUTNAMI_GITLAB_API_URL` names, for tests.

2. **Settings name the project; the environment holds the credential.**
   `settings.project` is the project's full path (`group/subgroup/name`),
   `settings.host` the instance (`gitlab.com` unless set). The credential is
   found the way `glab` finds it, so a developer logged in with `glab` and a
   pipeline both work without configuration:
   1. `GITLAB_TOKEN`, for the host `GITLAB_HOST` names, `gitlab.com` when it is
      unset: a workspace bound to one instance never receives the token of
      another.
   2. `glab config get token --host <host>`: the token `glab auth login`
      stored. GitLab's CLI has no `auth token` command; this is the command
      that prints the stored token alone.
   3. Inside a GitLab pipeline (`GITLAB_CI=true`) whose `CI_SERVER_HOST` is
      the bound host, `CI_JOB_TOKEN`, sent in the `JOB-TOKEN` header. GitLab
      lets a job token list and read issues and nothing more, so with it
      every write answers `denied`; a pipeline that writes sets
      `GITLAB_TOKEN`.

   Every other token goes in `Authorization: Bearer`, which GitLab accepts
   for personal, project and group access tokens and for the OAuth token
   `glab` may store. Without a credential every call answers `denied`
   (`gitlab.unauthenticated`) and sends nothing. The runtime removes the
   token, and every string shaped like a GitLab token (`glpat-`, `gloas-`,
   `glcbt-` and the other published prefixes), from everything it writes. A
   401 or 403 names where the token came from, never the token.

3. **A reference is the issue's `iid`, qualified by host and project.** A
   task is `{"source": "gitlab:<host>/<project path>", "id": "<iid>"}`, all
   lowercase, and its `key` is `#<iid>`. The host is always present, unlike
   GitHub's source: self-managed instances are common, and a path never
   becomes ambiguous with a host. A source longer than the contract's 200
   characters is a settings error.

4. **States are labels, configured as the GitHub provider's are.**
   `settings.states` lists the labels of `open`, `in_progress`, `blocked`,
   `done` and `canceled`, and `settings.stateLabelPrefix` makes every label
   with that prefix a state label. An opened issue is `blocked`,
   `in_progress` or `open` by the first of those whose label it carries.
   GitLab closes an issue without a reason, so a closed issue is `canceled`
   when it carries a `canceled` label or GitLab closed it as a duplicate
   (`_links.closed_as_duplicate_of`), and `done` otherwise. A transition
   removes every state label, applies the target's first label, and closes
   or reopens the issue in the same request. `canceled` therefore needs a
   label: without one it is `unsupported`, as `in_progress` and `blocked`
   are.

5. **A revision covers what a task shows.** It is `v` and the first 32 hex
   digits of the SHA-256 of the JSON array of title, description, state,
   duplicate link, sorted labels and sorted assignee usernames. A comment
   does not move it; `updated_at` would. GitLab has no conditional issue
   write, so `update`, `transition` and `assign` compare the revision with
   the issue read just before the write and declare `preconditions: checked`.

6. **The idempotency key is a hidden comment of the description, and GitLab's
   lists find it at once.** As on GitHub, `create` ends the description with
   `<!-- putnami-idempotency k=<sha256 of the key> d=<sha256 of the request> -->`,
   which GitLab's Markdown does not render. Unlike GitHub's, GitLab's issue
   lists read its database, and its load balancer keeps a token's reads on
   the primary database after its writes, so an issue appears in the list as
   soon as its create is answered. The lookup therefore reads the newest
   issues the credential's account opened in the project, `keyScanPages`
   pages of 100 (3 unless set), then GitLab's issue search for the key, and
   needs none of the GitHub provider's reads by number. Only an issue the
   account opened answers for a key. The same digest returns the issue
   (`created: false`), another digest is a conflict.

7. **A write is sent once and read back when its outcome is unknown.** After
   a lost answer or a 5xx, `create` looks its key up again and `update`,
   `transition` and `assign` read the issue again, up to three times. A write
   the reads cannot establish answers `unresolved`, names the read or the
   keyed repeat that settles it, and is never sent again.

8. **`find` reads from a creation time, not from a page number.** GitLab
   sorts a project's issues by creation time to the microsecond, then by id
   descending, shows the time to the millisecond, and cannot sort them by
   `iid`; an offset page also moves whenever an issue before it is closed or
   deleted. So every read asks for the issues created at or after the
   millisecond of the last issue considered (`created_after`), oldest first,
   and the cursor is that millisecond with what was already considered at it:
   `t<millisecond>.i<id>-<id>…`, the ids, while there are at most 48, and
   `t<millisecond>.n<count>` past 48, the count, read at that offset. A read
   skips every id the cursor names wherever it sits in GitLab's answer, and
   each read lists at least one issue not yet considered, so a traversal
   always ends. An issue present throughout a traversal is listed once,
   whatever page sizes the caller asks for, ties in creation time and issues
   closed meanwhile. The one exception is a tie of more than 48 issues in one
   millisecond: there the cursor counts positions, and closing issues of that
   tie during the traversal can hide others of it.

9. **A body is data, never a command.** GitLab runs a quick action from any
   description line that starts with a slash (`/close`, `/label`, `/move`)
   and stores the description without it, so a body could change a state
   behind `transition` and its revision check. Outside fenced code, where
   GitLab runs none, `create` and `update` insert a zero-width space before
   such a slash, and before a zero-width space that starts a line; GitLab
   stores the line and runs nothing, and a read removes exactly one, so a
   task's body is what the caller wrote. A transition to `done` of an issue GitLab closed as a duplicate
   first reopens it, which clears the duplicate link, then closes it. A label
   named `None` or `Any`, keywords of GitLab's label filter, is matched by the
   provider rather than sent.

10. **`assign` is offered; `link` and `claim` are not.** `assign` resolves each
   username to a user id (`@me` is the credential's account) and replaces
   the assignees; GitLab keeps only one on a tier without multiple
   assignees, and the answer says who is assigned. GitLab's issue
   hierarchy is an epic or a work item parent, outside the issues REST API,
   so `link` waits for a decision on work items; nothing enforces an
   exclusive claim.

## Consequences

- A workspace that moves from GitHub to GitLab keeps its `states` mapping and
  changes `repository` into `project` and the provider name.
- Two concurrent creates with one key can each open an issue, and a key
  repeated after the account opened more than `keyScanPages` × 100 issues
  relies on GitLab's search to find the first one.
- A pipeline with only its job token reads tasks and writes none.
- A `canceled` task is a closed issue with a label: an issue someone closes
  in GitLab's interface without that label reads as `done`.
- `bin/live-test` runs the contract scenarios against a real GitLab project
  with the owner's credential; it is never part of the gate.
