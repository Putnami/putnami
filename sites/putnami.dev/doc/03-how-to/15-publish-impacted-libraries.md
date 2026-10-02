# Publish impacted libraries coherently

You want to publish only the artifacts affected by a change, without inventing
versions for unchanged ones or rebuilding the entire workspace. A release set
makes that sparse publication one immutable, closed snapshot even when its
members have different versions.

## Publish the impact closure

```bash
putnami publish --impacted --channel canary
```

There is no bootstrap step. An empty channel resolves to a null head, every
member is selected, and the release expects no head — the first publication of a
channel is an ordinary publication.

Putnami resolves the channel once, before packaging, then republishes every
member whose **selection fingerprint** differs from the head's record, plus
every member that depends on one. Everything else is inherited from the head
verbatim: neither packaged nor uploaded, and still referenced at its exact
previous version. The resulting snapshot is closed — every internal dependency names a
version that exists in the same release set.

A member's selection fingerprint is the execution cache key of its package task
minus the embedded version. It moves when the sources move, and also when the
packager, the toolchain, a task contract, or any upstream task moves — none of
which a tree hash would catch.

For example, a downstream-only change produces this mixed-version set:

| Library | Version in the new set | Packaged and uploaded? | Exact internal dependency |
|---|---|---|---|
| `@putnami/core` | `0.1.0-aaa` | No | — |
| `@putnami/web` | `0.1.0-bbb` | Yes | `@putnami/core@0.1.0-aaa` |

If `@putnami/core` changes instead, Putnami selects it and repackages
`@putnami/web`, so the downstream dependency points at the core version in the
new snapshot.

## Members are keyed by ecosystem and coordinate

A member is `(ecosystem, coordinate)`, not a project. One project yields one
member per ecosystem and coordinate it declares, so a service that publishes a
Go module, an image, and its configuration appears three times in the same
snapshot, and each of those members is selected independently.

Which ecosystems exist is not built into the CLI: every installed extension
declares its own ecosystem profile in its `putnami.extension.json` — `npm` by
`@putnami/typescript`, `go` by `@putnami/go`, `oci` by the extension SDK's
shared publisher — and the coordinator admits exactly those.

## Advance several channels at once

```bash
putnami publish --impacted --channel canary,staging
```

Impact is measured against the **first** head; every listed channel is then
advanced to the same snapshot, each compare-and-swapped against its own resolved
head, in one transaction. A conflict on any one of them writes nothing on any of
them and the error names every head that moved.

## Publish a tag as one cohort

When HEAD carries its line's tag, `putnami publish` needs no selection flag and
no `--channel`:

```bash
git push origin ts/v0.3.0     # CI checks out the tag and runs putnami publish
```

Every member of that line is republished at the tag's version, and the release
creates an **immutable** channel named after the tag in the portable encoding
(`ts/v0.3.0` becomes `ts-v0.3.0`). That channel accepts one head and refuses
every later move, so a tag names one snapshot forever. A dirty tree is refused,
and `--scope <line>` names the line when HEAD carries several tags.

## Keep the publish credential out of your jobs

When your credential provider supports `publication-v1`, turn on its `publish`
purpose and the whole publication runs through that provider:

```bash
putnami publish --impacted --channel canary --providers publish
```

1. Putnami resolves the channels through the provider and plans the set.
2. Your tests and builds run. When every task that does not publish has
   succeeded, Putnami opens the plan with the provider, once.
3. Each publication job packs its members into a private outbox. It receives no
   registry credential, so no script or tool it starts can read one or upload.
4. Putnami hashes every packed file again, checks each member against the
   plan, and uploads the npm tarball, the Go module zip, the OCI image, or the
   release archive, config, migration or site-content bundle itself, with the
   publish credential the provider issues after the open.
5. Putnami releases the set over the same session: the opened plan, the heads it
   moves from, and the digests it uploaded.

Channels only move forward. Opening the plan states, for each channel, the
commit its head was published from and whether that commit is an ancestor of
yours. The provider refuses a head that is not (`not_forward`), and nothing is
uploaded. It refuses a release whose artifacts the registries do not store
(`artifact_missing`). Each refusal names its code and moves no channel.

A release archive, config, migration or site-content bundle uploads as a
private, immutable version of the Put registry, and only the release moves its
channel. When a selected one has no Putnami upload, because its job uploaded it
itself or packed nothing, the run fails and the release moves no channel.

A publication job served from a cache packed nothing in this run, so the
release refuses the run and names the job. The publication tasks of the
TypeScript and Go extensions never cache their result; a publication task you
declare yourself must not cache either.

The plan names the commit, its members and the channel heads, and nothing of
the machine that runs it, so a run on your machine and a hosted run of the same
commit open the same plan and release the same set.

## Treat failure as no publication

Sparse release-set publication fails closed when any of these is not proven:

- one release-set provider, or a credential provider that supports
  `publication-v1`, is available;
- every listed channel resolves to a valid answer for the workspace;
- the impact selection contains every downstream that must be repackaged;
- every selected artifact is uploaded and verified by its SHA-256 digest;
- the final snapshot is closed and stored immutably;
- the compare-and-swap succeeds on every listed channel.

A partial upload or a compare-and-swap conflict is not a successful publication:
every channel head is preserved. Fix the cause and replan from the current
heads; the command never falls back from `--impacted` to `--all`.

Without a release-set provider, `putnami publish --all` still publishes every
member to the registries the workspace declares, with git-derived versions.
`--channel` is refused, and `distribution` and `envs` in `putnami.ci.json` fail
`putnami ci validate`.

## Keep channel selection separate from delivery

A channel such as `canary` is a mutable pointer. A release-set reference is an
immutable content address. Successful publication emits one typed
`data.releaseSet` payload:

```json
{
  "protocolVersion": 2,
  "namespace": "putnami",
  "ref": {
    "id": "rs_<64 lowercase hex>",
    "digest": "sha256:<same 64 lowercase hex>"
  },
  "channels": {
    "canary": { "ref": { "id": "rs_<64 hex>", "digest": "sha256:<64 hex>" }, "generation": 12 }
  }
}
```

`channels` reports the head each advanced channel now points at, with the
monotone generation the provider stamped. A registry applies a projection only
when the generation increases, so a delayed event can never restore an older
projection and a retry is idempotent. `putnami channel status <c>` shows the
desired head against what each registry has actually applied, and exits `1`
while any is behind.

CI must carry both `ref.id` and `ref.digest` from that result into deployment.
Deployment consumes those exact values and must not resolve `canary` again: if
another publisher moves it between publish and deploy, the deployment still uses
the snapshot the publish proved.

The same distinction applies when upgrading a workspace:

```bash
putnami upgrade --channel canary          # follow the channel on every registry
putnami upgrade --release rs_<64-hex> --namespace putnami # one immutable release set
putnami upgrade --version 0.1.0-abc123456 # one exact version
```

`--channel` reads the native projection each ecosystem publishes — an npm
`dist-tag`, a Go `@v/<channel>.info` query resolved per module, the put
registry's channel projection — so it needs no release-set provider and no
namespace, and any workspace can use it. A channel is mutable, so a channel that
moves mid-run can mix two publications across ecosystems; the release writes
every projection of a channel in one event with one generation to shrink that
window. `--release` is deterministic even if a channel moves later; it takes the
publishing `--namespace` explicitly and needs the release-set provider. See
[Upgrade Putnami](/docs/how-to/upgrade-putnami) for workspace update details.

## Promote and roll back without rewriting artifacts

Release sets and their member artifacts are immutable, so promotion and rollback
are the same gesture — move the channel:

```bash
putnami channel set latest --from canary        # promote the head canary points at
putnami channel set latest --from rs_<64 hex>   # roll back to an exact snapshot
```

`channel set` is one provider call: no publisher runs, no artifact is uploaded,
no branch is checked out, and the move is compare-and-swapped against the
channel's current head. A channel the repository declares `protected` in
`putnami.ci.json` moves only this way — `putnami publish --channel latest` is
refused before any provider call.

Roll a consumer back by selecting a previously known release-set id with
`upgrade --release <id> --namespace <namespace>`. Never overwrite or rebuild an
old snapshot.

You now have sparse publication that preserves unchanged upstreams, repackages
required downstreams, and hands deployment one exact immutable release set.
