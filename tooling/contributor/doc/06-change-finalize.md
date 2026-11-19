# Publish a proposal

`putnami change finalize`, which the `@putnami/contributor` executable serves,
publishes the current branch as a proposal through the bound `proposals`
provider, and with a task, delivers that task through the `tasks` provider.
It publishes a draft, or a clean committed change with current proof. It runs
no local gate:

```bash
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --draft
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --verification-file .context/dossier.json
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --task-source <source> --task-id <id> --gate ci --workflow-file .context/workflow.json
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --task-source <source> --task-id <id> --partial-delivery --gate ci
```

Every run needs `--base`, `--branch`, `--title` and `--body-file`. `--branch`
must be the current branch. The command pushes it to the `origin` remote and
resolves the base from `origin/<base>`, so `origin` must be the repository the
proposals provider manages.

A branch you rebased after a push publishes like any other. A push that
fast-forwards the remote branch goes through as is. When `origin` refuses it
as not a fast-forward, as after a rebase, the command pushes again with a
lease on the head this checkout last pushed, read from the reflog of
`refs/remotes/origin/<branch>`: it replaces the remote branch only while
`origin` still holds that head. When someone else pushed the branch since,
the command refuses, writes nothing, and names the head it expected, even
after a fetch. Fetch, make your branch contain `origin/<branch>`, for example
with `git rebase origin/<branch>`, then run it again: the push then
fast-forwards. The lease needs the reflog of the remote-tracking ref, which
git keeps by default, and a fetch configuration that tracks the branch under
`refs/remotes/origin/`. A checkout whose reflog records no push of the
branch only fast-forwards.

## Modes

- **Draft.** `--draft` publishes a draft and accepts no proof. A branch with
  no commit ahead of the base gets an empty commit that carries the title; the
  squash drops it. Run it again at each milestone to push the committed head
  and refresh the title and body. A staged change refuses the run.
- **Local ready.** `--verification-file <dossier.json>` publishes a ready
  proposal when `putnami change verify --record` gives the dossier local ready
  assurance for the committed head. The tree must be clean.
- **Hosted ready.** `--gate ci` marks the proposal ready, which starts the
  hosted checks, and waits for every check that
  `verification.ciGate.checks` names on the pushed head. A failing or
  unsupported check returns the proposal to draft and fails the run. Checks
  still pending after `verification.ciGate.timeoutMinutes` fail the run, and
  running the command again keeps waiting. The policy must name distinct
  checks and set `publication.draft` to `false`.
- **Merge queue.** `verification.ciGate.mergeQueue` lists the base branches
  that merge through a merge queue, such as `["main"]`. Toward a listed
  `--base`, `--gate ci` takes that branch's queue as its gate. The queue runs
  the checks on its own commit, and the head's gating check stays pending
  until the proposal enters the queue, so the command waits for no check on
  the head. It marks the proposal ready, reads it back open at the pushed
  head, reports `merge queue of <base>` as its gate (on stderr, and as `gate`
  in the result document), and goes on to the verification comment, the
  task's transition and the auto-merge request. A failing queue run removes
  the proposal from the queue; the command does not see it, and the proposal
  stays ready. Absent, `null` or empty, the list names no base; any other
  value, an empty name or a repeated one stops the run before any write.

A ready publication takes exactly one of `--verification-file` and `--gate ci`.
`--proof <text>` adds the local proof to the verification comment.

## Auto-merge

With `publication.autoMerge` set to `"squash"`, a ready publication ends by
asking the proposals provider to turn auto-merge on, once, through the
optional `proposals.automerge` operation. The request names the published
head, so a head that moved since is refused. It comes last: after the ready
upsert, the gate (the dossier, or the named hosted checks read again), the
verification comment and the task's delivered transition. Under a merge-queue
gate, the gate is the queue itself, which runs once auto-merge enters the
proposal into it. The provider then
merges the proposal, or enters it into the merge queue, once the repository's
rules allow; the command merges nothing. For `@putnami/github-collaboration`,
this is what `gh pr merge --auto --squash <number>` does.

The command sends no request for `--draft`, for a policy with
`publication.draft: true`, for `--partial-delivery`, or for a proposal the
branch already merged. The value is `"squash"` or `false`, and `false` by
default; any other value stops the run before any write. The setting grants no
authority: the run publishes only because the user authorized it, and
auto-merge follows from that publication.

A refusal never fails the run. When the provider refuses (the repository does
not allow auto-merge, the credential lacks permission, or the proposal could
merge at once), or offers no `automerge`, the command reports it, leaves the
proposal ready, and exits 0.

## Existing proposals

Before it seeds, pushes or writes, the command reads the branch's proposals in
every state, following every page of the answer.

- **An open or draft proposal** is the one to update, whatever merged or
  closed proposals the branch also has.
- **A merged proposal at the current head**, with no open one, means the work
  is delivered. The command reports the proposal, exits 0, and seeds, pushes,
  writes and transitions nothing.
- **A closed proposal, or a merged one at another head**, with no open one,
  fails the run and names each such proposal. The command opens no
  replacement. Reopen the closed proposal, or publish the new commits from
  another branch.

## Tasks and partial delivery

`--task-source` and `--task-id` name the task the proposal delivers, and go
together. A normal delivery adds the policy's `publication.taskReference` line
to the body and moves the task to `states.delivered` once the proposal is
ready. Do this only when the mandated outcome is complete.

A `taskReference` with `{url}` works with every provider. One with `{id}`,
such as `Closes #{id}`, closes the task only when the proposal lands where the
task lives, so the command renders it only when the proposals binding is bound
to the provider that serves the tasks binding, both bindings locate the same
place, and that place spells the task's source. A place is the binding's
`host`, or the code host's default host, and its `repository` or `project`. A
GitHub source reads `github:<owner>/<name>`, or `github:<host>/<owner>/<name>`
on another host; a GitLab source reads `gitlab:<host>/<project path>`. Any
other binding stops the run before any write: use `{url}`.

`--partial-delivery` publishes one unit and leaves the task open: the body
refers to the task without closing it, and the task keeps its state. The
title, the body and every nonempty branch commit must hold no closing
directive.

`--gate ci` with a task needs `--workflow-file <workflow.json>`, an immutable
workflow record. The hosted checks never stand for a unit's native proof, and
neither does a merge queue: a queue gate promotes no unit and writes no
receipt, and a complete delivery still needs a record that the native
verifier finds complete once ready.

Without `--partial-delivery`, the command publishes only when that record
makes the outcome eligible. Each current unit must be at `candidate`, or at a
later stage with its receipt, and no requirement may be `later`. The record's
`proof` must name a local producer session on the same tree and its sibling
`spec-verification.json`, which shows each current requirement `verified`. A
`--projects` gate run on the committed tree, after the last commit, gives that
session. Otherwise the command stops before any write and names the blocker,
for example `workflow ready needs native producer proof`.

With `--partial-delivery`, the record must be valid. Each current unit at
`candidate` whose native checks pass becomes ready, and the result carries its
receipt. When no unit is eligible, for example a unit still at `building`, the
proposal still publishes with the non-closing task reference, with no ready
unit and no receipt.

A partial delivery needs no workflow record when every changed path belongs to
projects that declare `featureAuthority.none` and author no feature, both on
the branch and at the base. The hosted checks on its head are then its
evidence. putnami names the projects that own each changed path, with the
rules its impact analysis uses.

A complete delivery of a change that carries no feature requirement, such as
an ADR with a `decisions/<id>.json` file, passes a workflow record that names no source and
one unit with no requirement, at `candidate`. The hosted checks complete that
unit, and the command closes the task. A change whose files no project owns,
such as one that touches only `putnami.workspace.json` and `AGENTS.md`, scopes
that unit to the workspace root: its `projects` entry is `{"id": "/",
"manifest": <ref of putnami.workspace.json>}`. The record is refused when the change
edits a native spec, an external spec a feature manifest names, or a feature
manifest.

## Screenshots

A ready publication, local or hosted, of a change that touches a page of a web
project needs the screenshots of its head
([Capture the screens of a change](09-change-screenshots.md)). A web project
commits `schema/http-routes.json` with pages. The command asks the putnami
process for the projects that own each changed path, adds each project whose
committed route inventory at the head has a page that proves a requirement
the change adds or modifies, reads each one's route inventory at the merge
base and the head, and selects the touched pages with the rule
`putnami change screenshots` applies. A change that only removes pages needs
none.

```bash
putnami change finalize --base main --branch topic --title "feat(web): a lighter task list" --body-file body.md \
  --verification-file dossier.json --screenshots .putnami/42/screenshots/screenshots.json
putnami change finalize --base main --branch topic --title "refactor(web): rename the layout class" --body-file body.md \
  --gate ci --waive-screenshots "a class rename that renders the same"
```

- `--screenshots <manifest>`, once per touched web project, names the
  manifest `putnami change screenshots` wrote. It counts when it is a version 2
  manifest, its `headSHA` is the published head, its `project` is the touched
  project, it holds the after captures of every touched page in both viewports
  and both schemes, each touched page names in `proves` each touched
  requirement it proves, each page it lists holds its captures on each side it
  exists on, and each file next to it holds the bytes its `sha256` records. A
  version 1 manifest names no requirement: capture again. A relative path
  resolves against the workspace root.
- `--waive-screenshots <reason>` publishes without them. The reason is one
  line of at most 300 bytes.

Otherwise the command stops before any write and names, for each project, the
pages without a manifest or what the manifest does not prove. The verification
comment gains a `screenshots:` line: the accepted manifests with their counts,
`waived: <reason>`, or `no page touched`, which adds `, so the screenshots
given are unused` when the run had a waiver or a manifest; the result document carries the
same text as `screenshots`. A draft takes neither flag and needs no
screenshots, and the two flags exclude each other.

### Upload

The command sends each manifest it accepted to the bound proposals provider,
so a reviewer sees the screenshots on the change
([ADR 0015](adr/0015-finalize-uploads-its-screenshots-through-the-proposals-contract.md)).
It calls `proposals.screenshots` once per manifest, in project order, with the
published proposal, the published head and the manifest's path. The upload
runs after the proposal is published and its gate passed, and before the
verification comment. A waiver, a draft and a change that touches no page
send nothing.

| The provider | The run |
|---|---|
| Stores the manifest | Prints `uploaded the screenshots of <project>: <n> captures, <n> files sent` on stderr. A provider that already holds a file does not receive it again. |
| Does not offer `screenshots`, such as `@putnami/github-collaboration` | Sends nothing and warns on stderr. |
| Answers `unsupported`: `@putnami/cloud` under a machine token (`PUTNAMI_CLOUD_TOKEN` with no signed-in session), or with a Putnami Delivery that does not serve the route | Stops uploading and warns on stderr with the provider's message. |
| Answers anything else, such as a head that is not a commit of the change, or a file too large | Fails with the provider's message, before the verification comment and the task's transition. Fix the cause and run the command again: it sends only what the provider still lacks. |

A warning changes neither the verification comment nor the exit code. The
verification comment is the same with and without the upload. The result
document carries `screenshotsUpload`: `manifests` (`project`, `captures` and
`uploaded` for each one stored), or `skipped` with the reason nothing was
stored.

## Text rules

The title uses Conventional Commits. A `feat` or `fix` title is refused when
every changed path is a test. The body must not be empty and must hold no
Markdown heading. It stays under 65536 bytes, and under
`publication.bodyMaxBytes` when the policy sets it. The command joins the lines
of each paragraph, because the body becomes the squash commit message.

Before it writes anything, the command removes each line of the body that
names an agent, in one of the three forms the `agent-attribution` step of
`validate` refuses: "Generated by" or "Generated with" followed by an agent's
name, a `Co-Authored-By` trailer for an agent, and a link to an agent's
session or the trailer that carries one. Agent names match as whole words, as
the step reads them, and a form is read line by line: a footer wrapped over two
lines loses only the line that matches, so write it on one line or delete it.
It prints each removed line on
stderr, `change finalize: removed line <n> of the body, which names an agent
(<kind>): <text>`, and lists it in `stripped` of the result document. The body
file stays as written. A body left empty stops the run, and so does a title,
a proof or a branch commit that names an agent. With
`verification.language` set to `en`, the title, body and proof must carry no
non-English signal.

The command reads the proposal back and requires the exact title and body it
sent. When the provider or its host appended text to the body, the error names
that text. When the text names an agent, for example the attribution footer a
Claude Code cloud session appends after each write, the error says to publish
from a local session.

## Output and exit codes

The command prints `PROPOSAL_REF=<json>`, then `PROPOSAL_URL=<url>` when the
proposal has a URL, then `CHANGE_URL=<url>` when the proposals provider
answers the proposal's `pageUrl`: the link to the change in the provider's
own console, such as its change page on putnami.app, on draft and ready runs
alike. When the policy sets `publication.autoMerge` and the run
sent the request, it adds `AUTO_MERGE=squash` when auto-merge is on, or
`AUTO_MERGE=off` with the reason on stderr. With `--output=json`, `jsonl` or `cloud-logging`, it
prints the result document on one line instead: the proposal reference and
URL, `changeUrl` when the provider answers a page, the head commit, the proposal state, the gate, whether the labels and
assignees read back, `screenshots` for a ready publication, `screenshotsUpload` when it accepted a manifest, for a hosted workflow, its digest, ready units and
receipt, `autoMerge` (`method`, `enabled` and the `refusal`) when the run
sent the request, and `stripped` (`field`, `line`, `kind` and `text` of each
removed line) when the body lost a line. For a proposal already merged, the state is `merged`, the text output
adds `PROPOSAL_STATE=merged`, and stderr says that nothing was published.

It exits 0 when the proposal is published or already merged, auto-merge on or
refused, 1 when the run
fails, and 2 for a usage error. A failure prints `change finalize: <error>` on
stderr. `Ctrl-C` stops the run, and putnami exits 130. The run has a 90-minute
deadline: when it passes, the run fails with `context canceled`, a proposal
that waits for hosted checks stays open and ready, and running the command
again resumes the wait. A provider write whose outcome is unknown stops the
run with the provider's reconcile hint, and the command never retries it. The
command takes no project selection, and refuses `--projects`, `--all`,
`--impacted`, `--baseline`, `--tag`, `--exclude-tag` and `--exclude`.

## Launch

putnami launches the command interactively and opens a host RPC channel for
it. The executable reaches the providers, the owners of changed paths and
restored verification reports through that one channel. A build job, a
provider tool, a hosted run and a process putnami did not launch have no
channel, so the command fails there and names the launch it needs. No Git
command it starts receives the channel.

[ADR 0006](adr/0006-the-contributor-executable-serves-change-finalize.md)
records how the executable serves the command and where it differs from
`putnami proposals finalize`, the core command it replaced.
