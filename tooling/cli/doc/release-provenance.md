# Release Provenance Policy

This policy defines the provenance a Putnami CLI release must retain and the
integrity claim the public installer is allowed to make. It applies to every CLI
archive promoted to a public channel. The release owner must stop promotion when
any required record or digest is missing; an exception may not turn an
unidentified artifact into a release.

## Digest algorithm and authoritative source

The only digest algorithm accepted for release artifacts is **SHA-256**. The
canonical representation is 64 lowercase hexadecimal characters, optionally
carried with one of the prefixes the installer normalizes.

The authoritative digest source for a registry install is the successful HTTPS
download response that carried the artifact bytes. `X-Integrity` takes
precedence; otherwise the installer reads the `sha-256` member of an RFC 9530
`Digest` header. The registry response is the current release manifest for that
one `(version, operating system, architecture)` artifact; Putnami does not yet
publish a separate immutable release-manifest file. An explicit
`--download-url` instead uses the operator-supplied `--sha256`, and refuses a
conflicting advertised digest.

The installer computes SHA-256 over the downloaded bytes and installs only when
it matches the authoritative value. A missing value, malformed value, missing
hash tool, or mismatch is a refusal unless the user explicitly opts into the
documented unsafe-install escape hatch. The release smoke never uses that escape
hatch: it independently checks the channel response and requires the installer
to report the successful comparison.

## Builder identity and release record

The release record must identify the builder before a candidate is promoted. It
records:

- the full source commit and confirmation that the build tree was clean;
- the release version and candidate channel;
- the exact Putnami CLI version that ran the package and publish plan;
- the builder identity: the external release-execution run and its actor when
  that plane is configured, or the named human release owner for a manual run;
- every target artifact's operating system, architecture, resolved version, and
  authoritative SHA-256; and
- the five target-specific smoke results (four macOS and Linux, one
  `windows/amd64`) and the release owner's approval.

The builder is the identity that executed the approved package plan, not the
registry that later serves its output. A retry is a new builder run and gets a
new record. Promotion must use the bytes and digests from the approved run; it
must not rebuild them under the same version.

## Signature policy

Current Putnami release artifacts are unsigned. No detached signature,
certificate, or transparency-log entry is produced, and neither the installer
nor `putnami upgrade` claims to verify one. SHA-256 comparison proves that the
download matches the bytes the authenticated registry advertised; it is not an
artifact signature and must never be described as one.

The installer's command map, `https://putnami.dev/install-commands.txt`, is not
signed either, and it carries no digest. It is served from the same origin as
`install.sh`, so it is trusted exactly as far as the installer. It only names
which extension provides a command; the CLI then installs that extension with
the same SHA-256 verification as any extension install. See
[Installing the CLI](22-installing-the-cli.md#the-command-map-is-trusted-as-far-as-the-installer).

Adding signatures is a future release-policy change. Before a release can claim
signed provenance, the signer identity, key or keyless trust root, signature and
attestation formats, transparency requirements, expiry and revocation behavior,
and fail-closed verification path must be specified here and implemented in both
install clients and the release smoke.

## Reproducibility

CLI archives are not currently promised to be byte-for-byte reproducible. The
archive packager uses the host `tar`/gzip implementation and retains staging
metadata that can vary between otherwise equivalent builder runs. A second
build is therefore diagnostic evidence only; its digest may not replace the
digest recorded by the approved builder or be presented as a reproducible-build
attestation.

The source commit, builder identity, Putnami version, target tuple, and SHA-256
make the shipped bytes traceable despite that limitation. A future
reproducibility claim requires deterministic archive metadata and a test that
independent clean builders produce identical bytes before this policy or a
release report may say the claim is met.

## Promotion, refusal, and rollback

Promotion copies or repoints channel metadata to the already-approved immutable
version; it does not rebuild or replace the versioned artifact. The candidate
does not promote when a target is missing, its registry digest differs from the
release record, its bytes differ from that digest, its embedded version differs
from the resolved version, or any target smoke is absent or failing.

Never replace bytes at an existing immutable version. For a bad candidate, stop
and leave the public channel on its previous version. For a bad published
release, the release owner repoints the CLI channel to the last known-good
immutable version, publishes a superseding patch, and records the bad version in
the release notes. Users can pin that previous version; its recorded SHA-256 is
still verified fail-closed. A digest mismatch or provenance gap is an incident,
not a reason to recompute a digest for the bytes currently being served.

The private-archive/public-root visibility switch is not a provenance step. It
remains the final separately approved human operation after the recorded
rehearsal says GO; this policy never changes visibility, rewrites history,
publishes, tags, or pushes.

## Evidence

- [`scripts/install.sh`](../scripts/install.sh) implements the fail-closed
  `X-Integrity`/RFC 9530 `Digest` SHA-256 comparison.
- [`scripts/smoke-check-release.sh`](../scripts/smoke-check-release.sh) checks
  the candidate response, installer verification, target binary, and resolved
  version before promotion. With `SMOKE_RUN_COMMAND`, it also runs the
  installer's run form against the published command map.
- [Installing the CLI](22-installing-the-cli.md) documents the user-visible
  trust boundary and refusals.
- [Releasing Putnami](../../../RELEASING.md) requires this provenance record in
  the release checklist and owns immutable rollback.
