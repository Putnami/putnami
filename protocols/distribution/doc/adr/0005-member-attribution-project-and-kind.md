# ADR 0005 — Member attribution: source project and artifact kind

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/distribution` (`protocols/distribution`),
  `go.putnami.dev/protocol/ci`, `putnami-extension-sdk`
  (`tooling/extension-sdk/releaseset`), `@putnami/cli`

## Context

A member is keyed by `(ecosystem, coordinate)`; the project that published it
is provenance, not identity. A consumer that holds only a channel head, such as
a control plane binding an environment's workloads, must still answer which
member is workload X's image, its configuration, its migrations. The
coordinate is no convention for that: a configuration coordinate
`ws/arbitrary-name` can belong to project `apps/service`, and one project can
publish several members in one ecosystem. The publisher knows the project and
the publish step of every member.

A member also records the commit it came from (`sourceRevision`) but not the
content. A squash-merge or a rebase rewrites the commit and keeps its git tree,
so a consumer cannot see that a republished member holds the same content.

Every consumer of a plan or a set decodes it strictly and refuses an unknown
member field.

## Decision

`ReleaseSetMember` carries three OPTIONAL provenance fields.

- `project`: the canonical logical id of the publishing project, matching
  `^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`, the character class of the ci
  protocol's project selector, without a leading slash. It is the logical id a
  selector names, not the physical path: folders transparent to identity are
  absent, and the grammar admits no parenthesis. A project id outside the
  grammar records no project; publishing never fails for it.
- `kind`: the closed role vocabulary `image`, `config`, `migration`, `doc`,
  `library`, `archive`, `deployment`. It says what the artifact is, not how it
  was built. An unknown kind is a hard diagnostic (`distribution.invalid_kind`),
  because a consumer selecting by role must never guess. `deployment` is a
  workload's deployment declaration: its resolved requirements and runtime as
  one document.
- `sourceTree`: the full lowercase hex git tree of the checkout the member was
  built from, `^[0-9a-f]{40}$`. A malformed value is a hard diagnostic
  (`distribution.invalid_source_tree`). SHA-256 repositories are out of scope,
  as for `sourceRevision`.

Rules:

1. **Optional means optional.** An empty value decodes as absence and is
   omitted from canonical bytes, so a set without the fields keeps its `rs_`
   reference.
2. **Part of identity.** All three fields are in the canonical bytes, in that
   order after `platforms`. A set that states them is a different immutable set
   from one that does not. The member key never changes.
3. **Attribution is the plan's.** `project` and `kind` are what the workspace
   declares, whether or not the run republishes the member. An opted-in
   publisher records both on every member the plan declares, carried-over
   members included. An unchanged member inherits its artifact record
   (version, digest, dependencies, revision, fingerprint, platforms, tree)
   verbatim and takes its attribution from the plan. The first opted-in
   publication over an unattributed head therefore moves the set's reference
   once, even when it republishes nothing.
4. **The tree is the publication's.** A republished member records the tree of
   the current checkout; an unchanged member inherits its head's tree with the
   revision it goes with, and the extension SDK holds it to the head's record.
5. **HEAD's tree, from a clean checkout only.** The publisher records
   `git rev-parse HEAD^{tree}`, because HEAD's checkout is what the package
   tasks read. It records no tree when the checkout has uncommitted or
   untracked changes, or when git cannot answer. It reads the tree again when
   it commits the set and drops it from republished members when the checkout
   no longer holds it. Publishing never fails because of this field. Using an
   equal tree to skip a republication is the consumer's decision.
6. **The protocol derives no kind.** The extension SDK owns the one mapping
   (`releaseset.KindFor`) from a declared package or publish step to a kind,
   falling back on the ecosystem when it admits one role (`oci` is an image,
   `npm` and `go` are libraries, `archive` is an archive). An unknown step and
   ecosystem produce no kind. A kind that no ecosystem implies, such as
   `deployment` in the `put` ecosystem, is emitted only for a member whose
   extension declares the step that maps to it: a package or publish step
   named `deployment` maps to the kind `deployment`, and `KindFor` consults
   the publish step first.
7. **Opt-in per repository, off by default.** The publisher emits
   `project` and `kind` only when `putnami.ci.json` declares
   `distribution.memberAttribution: true`, and `sourceTree` only with
   `distribution.memberSourceTree: true`. A reader built without a field
   fails the publication: an extension refuses the plan with `unknown field`,
   or drops the field and reports `head ref does not match canonical
   release-set bytes`. Turn an opt-in on only after every reader of the
   affected channels carries the field: the extensions the workspace pins, the
   release-set provider extension and its backend, every CLI that resolves
   those channels (publishing from any branch, `putnami channel`,
   `putnami upgrade --release` in consumer workspaces), and the extensions
   those consumer workspaces pin that read a resolved head. The CLI cannot
   observe the backend. Once a head carries a field, every later plan inherits
   it, so the declaration is one-way for every channel that head reaches.

   A new kind token follows the same order. The token is appended to the
   vocabulary, never inserted, and readers learn it first: a strict reader
   built before the token refuses a set that carries it with
   `distribution.invalid_kind`. Emission comes second and only through an
   extension's member declaration, under rule 6. The CLI maps the step to the
   kind, and every extension that runs in the publication validates the plan
   that carries it, so a workspace declares the step only after its CLI and
   every extension it pins know the token. An older extension anywhere in the
   publication refuses every later plan. Once a head carries a member of the new kind, every later plan inherits
   it, so the addition is one-way: the token is never renamed or removed.

## Consequences

- A consumer groups a head's members by project and binds image, config and
  migrations per workload with no CI run in the loop.
- A consumer can tell that two members with different revisions were built
  from the same content.
- A member published before the source-tree opt-in, from a dirty checkout, or
  from a checkout that changed before the commit carries no tree until it is
  next republished from a clean checkout.
- The conformance corpus carries valid fixtures for attributed members, for a
  deployment member, and for a set with and without a tree, invalid fixtures
  for an unknown kind, an unrepresentable project and an abbreviated tree, and
  keeps the golden reference of a set without the fields unchanged.
