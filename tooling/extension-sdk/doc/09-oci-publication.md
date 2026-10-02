# OCI publication evidence

An image publisher must report each verified publication to the native release
set. A successful push and verified reuse of an existing digest both emit a
`published-member` event. Packaging a local image, a dry run, or a failed or
unverified publication does not provide that evidence.

The member keeps the logical registry coordinate: its repository includes the
namespace and excludes only the registry host. A private broker's loopback
address is a transport endpoint, so it never replaces that coordinate in the
member. The artifact digest identifies the immutable image that was verified.

When the invocation carries a native release-set plan, publication uses the
image member's exact planned version and managed repository namespace. The
plan must select exactly one OCI member for the task's typed project identity
whose repository ends with the packaged image name. A missing, unselected,
foreign-project, or ambiguous member fails before credentials are resolved.
An explicitly configured managed namespace must match that selected repository.
The workspace name, registry namespace, and release-set namespace may differ:
`acme-cloud`, `acme`, and `cloud`, respectively, publish an image at
`oci.putnami.dev/acme/<image>` when that is its selected coordinate.
Publication does not substitute a session
version. Without a native plan, standalone publication uses the package's
deterministic content version for member evidence and continues to restrict
the managed target to the workspace-derived namespace.

That version is evidence identity, not a request to create a registry tag.
Native image publication through the private broker writes the manifest at its
digest and creates no content, session, version, or channel tags. Channel
projection remains the release's responsibility. The existing Docker
publication path's version-tag behavior remains separate from this native
image path.

## Dry run

A dry-run image publication pushes nothing and asks the registry one question
per image: does it already hold this reference. It sends one manifest HEAD,
through the registry client and the keychain of a real push, and emits one
`member-probe` event:

| State | When |
|-------|------|
| `absent` | The registry answers 404. |
| `identical` | The registry holds the manifest digest package assembled. The real publish reuses it. |
| `tag-move` | The managed registry `oci.putnami.dev` holds the version tag at another digest, and the real publish moves the tag. |
| `conflict` | The registry holds another digest at a reference the real publish cannot move, or, on a registry other than `oci.putnami.dev`, package assembled no image to compare it with. |
| `unverified` | The registry cannot be reached, refuses the request, or answers outside the protocol. |

A workload image is asked at its version tag, an image project at its digest.
The probe compares with the manifest in the project's package directory. A
dry-run package that assembles no image leaves the manifest of the last real
`package` in place, so the comparison uses that image; its digest is checked,
its version is not. An `identical` verdict says so: its reason reads "compared
with the image the last `package` staged; re-run `package` if the source
changed since".

A version tag held at another digest is a `tag-move` on `oci.putnami.dev`
alone, where a manifest PUT moves the tag: the real publish moves it to the
packaged image, and the orchestrator prints a warning that names both digests
instead of failing the run. On any other registry it is a `conflict`: the dry
run cannot tell whether that registry lets the publish move the tag, and a
registry with immutable tags, for example, refuses it.

When a workload holds no manifest and the invocation carries a release-set
plan, the plan's selected OCI members of the project supply the coordinate and
the version, and the probe asks for the version tag. A held tag is a
`tag-move` without a local digest on `oci.putnami.dev`, and on any other
registry a `conflict` that names the remedy: run `putnami package` for the
project without `--dry-run`, then the dry run again. An image project without
a manifest is not probed: its publish writes the content tag or the digest
package computes, never the plan's version tag, so the orchestrator lists its
member as not probed. The probe resolves the credential once for the host,
through the host-only seam the real push asks; the cloud decides what the
credential grants. The event marks an answer obtained without a credential as
anonymous. The task succeeds whatever the answer; the orchestrator fails the
dry run on `conflict` and `unverified`. A `member-probe` event is never
publication evidence.

The `dry-run-member-probe` requirement covers this contract, and the probe of
a put archive (`memberprobe.ProbeArchive`).

The existing `oci-publish-by-digest` requirement covers this contract. Its
`image-member-matches-native-plan` check binds emitted evidence to the planned
member, and `image-private-publish-writes-only-digest` checks the private
registry write boundary.
