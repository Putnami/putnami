# ADR 0002 — Proposals on GitLab merge requests

- **Status**: accepted
- **Scope**: `@putnami/gitlab-collaboration` (`tooling/gitlab-collaboration`)

## Context

[ADR 0001](0001-gitlab-collaboration-provider.md) serves the `tasks`
contract on the issues of one GitLab project. Without `proposals` a
GitLab-hosted repository cannot run the contribution loop: `change finalize`
finds, opens, updates and reads its proposal, waits on its hosted checks and
posts its verification as a review. This decision serves `proposals` on the
merge requests of the same project.

A merge request resembles a GitHub pull request, and the provider follows the
GitHub provider's proposal decisions
([GitHub ADR 0001](../../../github-collaboration/doc/adr/0001-github-collaboration-provider.md))
wherever GitLab allows it. GitLab departs in four places: a draft is a title
prefix, a review is not an object with a body, a check is a commit status, and
the merge method is a project setting.

## Decision

1. **A proposal is a merge request of the bound project, referenced `!<iid>`.**
   Merge requests and issues number separately in GitLab, so a proposal's
   `id` is `!<iid>`, GitLab's own merge request reference, and a task's stays
   the bare issue iid: neither contract ever reads the other's reference. The
   source is the tasks source, `gitlab:<host>/<project path>`, and the
   proposal's repository is the project path the settings name. Both
   contracts bind with the same `project` and `host` settings and the same
   credential.

2. **A head is a branch of the bound project; forks are not served.** A
   proposal is the merge request whose source branch, in the bound project,
   and target branch are exactly the request's head and base. A git branch
   cannot hold a colon, so a head written `owner:branch`, the GitHub spelling
   of a fork, is `invalid` and nothing is sent, and so is a request naming a
   repository other than the bound project. A merge request opened from a fork
   on a branch of the same name is another proposal and is never answered,
   by identity or by reference.

3. **One open merge request per exact identity, as on GitHub.** GitLab
   refuses a second open merge request for one source and target branch, so
   `upsert` finds the open one and updates it, or opens it; a create GitLab
   refuses because one exists, or whose answer is lost, reads the open merge
   request back and is `created` only when it carries what was sent. A
   requested `headCommit` must be the commit the branch points at, read from
   GitLab's branches API. GitLab moves a merge request's `sha` some time after
   a push, so a `find` or `upsert` naming a commit the branch already points at
   re-reads the merge request for about 10 seconds, then answers
   `head.lagging`, retryable. A revision covers the title as GitLab stores it,
   the description, state, draft flag, base, head, head commit, labels and
   assignees; a note does not move it.

4. **A draft is a title prefix.** GitLab's REST API has no draft parameter: a
   merge request is a draft while its title starts with `Draft:`,
   `[Draft]` or `(Draft)`. `upsert` writes `Draft: ` before the title of a
   draft and leaves it off a ready one, in the same request as the title and
   description, and a proposal's `title` is the stored title without that
   prefix, so the title a caller sent is the title it reads back. An `upsert`
   without `draft` keeps the merge request's draft state. A requested title
   that starts with a draft prefix would set the state behind `draft`, so it is
   `invalid` and nothing is written.

5. **Bodies are text.** GitLab runs quick actions from merge request
   descriptions and from notes (`/merge`, `/approve`, `/close`), so the
   description and a review's body are escaped exactly as task bodies are
   (ADR 0001, decision 9), and a read removes the escape. A description reads
   back byte for byte, which `change finalize` requires of the squash message.

6. **A review is a note, and an approval for `approve`.** GitLab has no review
   object that carries a body and a verdict. `review` posts one note holding
   the escaped body, a hidden verdict and commit
   (`<!-- putnami-review verdict=<verdict> commit=<sha> -->`) and the
   idempotency marker of ADR 0001, decision 6. Before it, `approve` approves the
   merge request with the reviewed head as GitLab's `sha`, so GitLab refuses an
   approval of a head that moved, and `request_changes` withdraws the account's
   approval; each is skipped when the account's approval is already in that
   state, so a repeat converges. A key is looked up among the notes the
   account wrote on the merge request: the same digest returns the note
   (`created: false`), another digest is a conflict, and a note someone else
   wrote never answers. A note whose post is lost is read back and, when not
   found, answered `unresolved` naming the keyed repeat. `status` lists every
   review note, by anyone, with the verdict it records, and every current
   GitLab approval as an `approve` review (`approval-<iid>-<user id>`): the
   notes are the history, the approvals what GitLab enforces. GitLab's
   reviewer states and the Request changes of its review panel are not read
   or written.

7. **Checks are commit statuses.** `status` reads the commit statuses GitLab
   keeps for the head commit, its latest one per name. They hold every
   pipeline job and every external status a CI system posts, so a hosted gate
   named in the workspace policy is found by name, as on GitHub. `success`
   passes; `failed` and `canceled` fail unless the job is allowed to fail,
   when they are skipped, as GitLab does not fail the pipeline for them;
   `skipped`, and a `manual` job allowed to fail, are skipped; every other
   status is pending, a blocking manual job included, since the pipeline
   waits on it. GitLab lists the statuses of every pipeline of the commit
   and hides only jobs retried within one, so the latest status of each
   name, by id, decides: a failure an earlier pipeline reported does not
   outlive a later success. The statuses are read newest first, at most
   1,000; past them the oldest are not read, and the summary's detail says
   so.

8. **Comments are notes, by id.** `comments` reads every note of the merge
   request (at most 2,000), keeps those people wrote, never a system note,
   and pages them by ascending note id: the cursor is `n<id>`, the last note
   listed, so every note present throughout a traversal is listed once. A
   review note lists its body without its markers and names itself as its
   review. A diff note keeps its path, and its line only when it is anchored
   to a line of the new file, at the head commit it was written on.

9. **Publication additions match the GitHub provider's.** `assignAuthor` adds
   the credential's account to the assignees and `inheritLabelPrefixes` adds
   the prefixed labels of the issues the description names; both only add,
   and a failure after the merge request was written is `unresolved`
   (`gitlab.incomplete`), naming it. GitLab keeps one assignee on a tier
   without multiple assignees: there the present assignee stays, the account
   is not added, and the answer lists who is assigned, as `tasks.assign`
   does. A scoped label (`key::value`) is exclusive on the tiers that have
   them, so of each key one label is added, the first the description names,
   and none when the merge request already carries that key: an addition
   never replaces a label. The references are GitLab's: `#<iid>` is an
   issue of the bound project, `project#<iid>` one of a project in its
   namespace, `group/project#<iid>` one of any project of the instance, and
   `https://<host>/<project>/-/issues/<iid>` the same on the bound host.
   `!<iid>` names a merge request and adds nothing, and a URL on another host
   is never read.

10. **Merge is optional, off, and sent once.** `merge` is `denied` before
    anything is sent unless `settings.allowMerge` is true. It merges only a
    merge request whose head is the expected commit and sends that commit as
    GitLab's `sha`, so a head that moves between the read and the merge is a
    conflict, as is a merge request GitLab refuses to merge. The merge method
    and the squash option are project settings in GitLab: `squash` asks for
    a squash and is `unsupported` where the project never squashes, `merge`
    is served only where the project creates a merge commit and `rebase` only
    where it fast-forwards, both `unsupported` where the project always
    squashes; nothing is sent then. GitLab squashes by the merge request's
    own squash flag, which a create sets from the project's default and a
    merge call never clears, and on paid tiers a protected branch's rule
    overrides both. When the merge request's `squash_on_merge` already
    differs from its flag and from what the method needs, a rule decides,
    and `merge` is `unsupported` with nothing sent. Otherwise a named method
    sets the flag to what it needs and reads back `squash_on_merge`; when a
    rule still decides otherwise, or the merge does not happen for any other
    reason, the flag is put back as it was before the answer, so an answer
    that merged nothing changed nothing, and a flag that cannot be put back
    is `unresolved` (`gitlab.incomplete`). GitLab's merge call itself sets
    the flag when it asks to squash, before it decides whether it can merge,
    so a refused squash merge puts back a flag that was off too. An
    `unresolved` answer after a flag write names the flag's earlier value.
    The project's `never` and `always` refuse on the project's setting
    alone, even where a protected branch's rule would relax them: merge
    without a method there. No method leaves
    the choice to the project. GitLab takes
    `should_remove_source_branch` in the merge call itself, so
    `settings.removeSourceBranch` deletes the source branch with no second
    write to compensate for.

## Consequences

- A GitLab-hosted repository binds both contracts to one provider and runs
  `build` and `change finalize` as a GitHub-hosted one does.
- A title that starts with `Draft:` cannot be published as a ready proposal.
- A `request_changes` review blocks nothing in GitLab; a project that needs a
  blocking verdict uses its approval rules.
- A merge request with more than 2,000 notes answers `comments` and `status`
  `unavailable`.
- `bin/live-test` runs the proposal scenarios against a real GitLab project,
  on branches it creates one commit ahead of the default branch; when it ends
  it closes the merge requests on those branches and deletes them.
