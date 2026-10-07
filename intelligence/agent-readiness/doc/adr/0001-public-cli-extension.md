# Public agent-readiness CLI extension

Status: accepted

The command and repository collector live in the public Putnami repository
as `@putnami/agent-readiness`, a separate extension. A reader can inspect the
collection and privacy checks without installing hosted Intelligence.

The extension uses public Putnami runtime and CLI contracts and the embedded
payload schema. Submission uses a generated client of the real anonymous provider operation,
with a transport that preserves the same bytes print-only mode exposes. Only
that operation and its schema closure form the public provider contract slice. Server-side scoring, storage, report pages and
hosted Intelligence stay in their existing service. The migration changes no
marker, threshold or payload field.

The extension owns the `agent-readiness` command group. The Intelligence
extension must relinquish that group when a workspace adopts both extensions.
Its historical `intelligence agent-readiness` spelling is outside this
extension's command vocabulary.

The source provider slice carries its generation configuration in
`.gen/clientgen/config.json`, the generator's required input path. This one
file is source-controlled explicitly; other generated build outputs remain
ignored. The generator owns the committed client package and manifest.
