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

The existing `oci-publish-by-digest` requirement covers this contract. Its
`image-member-matches-native-plan` check binds emitted evidence to the planned
member, and `image-private-publish-writes-only-digest` checks the private
registry write boundary.
