# Feature protocol conformance pack

`putnami.features.protocol-v1` is a pure, reusable wire-conformance pack. It
requires no service and no language runtime beyond the producer under test. The
pack ID names the pack, not a document version: each document in the corpus
carries its own `protocolVersion`.

Go producers call `conformance.RunManifest`, `conformance.RunEvidence`, or
`conformance.RunVerificationReport` on their emitted bytes. TypeScript producers
consume the same files listed in `vectors.json` and must reproduce each golden
byte-for-byte: declared field order, two-space indentation, UTF-8-compatible
JSON escaping, and one trailing newline.

`human-authored-features-v2.golden.json` pins the current manifest wire, the one
that may carry an executable `verification` criterion. The version 1 vector is
kept beside it unchanged, because manifest readers must keep accepting both and
the repository has not migrated a single committed manifest.

`go-typescript-verification.golden.json` is a run-scoped result envelope rather
than a committed artifact: it exists so every producer emits, and every reader
accepts, the same bytes before any of them can gate on one. It also pins what an
observation may *not* say — no issuer, project, source binding, threshold, or
objective verdict.

`human-authored-spec.golden.json` has no producer on purpose: nothing mints a
spec, because a spec only details a feature some other declaration already
authored. Its `authors` value is therefore `human`, and the vector exists so
every *reader* — `conformance.RunSpec` here, and any other runtime later —
proves it re-emits a hand-written spec without touching a byte.

`generated-evidence.golden.json` is the output of the shared generated-evidence
resolver, not a hand-written document. Both languages compose the same semantic
scenario — one authored `coded` requirement, two mapped contributions, one
published contribution nobody mapped, and one owned by a dependency — and must
reproduce these bytes. It therefore pins what a producer must *not* emit as
well as what it must: the unmapped contribution stays unclassified and the
dependency-owned one is unclaimable, so a language that widened its association
rule fails here rather than in a repository count.

The provisional `go-typescript-native-design` vector is produced by real Go
and TypeScript module composition. Its tests remove only language-specific
source provenance after asserting that provenance separately, then compare the
remaining node identities, properties, and relationships byte-for-byte.

`python-authored-evidence.golden.json` was authored using Python's standard
JSON data model and then committed in the shared canonical form. It proves the
framework-neutral wire shape without adding a Python application lifecycle or
producer runtime. Go validates and re-emits it in the normal gate; a future
Python build-time helper can adopt the same vector without changing the wire.

Aggregate semantics are checked by `features.ValidateRepository`, because a
single fragment cannot decide workspace-wide feature/evidence uniqueness or
resolve cross-project relations and requirements. Spec aggregation is checked
the same way by `features.ValidateSpecRepository`.
