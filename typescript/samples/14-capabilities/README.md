# Capability manifest v2 and native feature graph

Proof workload for the TypeScript capability-manifest producer. Its application
retains the established `capabilities/source-bound-manifest` feature ID, declares
it once, and combines the contribution
paths that `@putnami/application` can observe during generate:

- registry-backed config, including a sensitive field;
- config and migration-source discoverers;
- a migration source and its database infra requirement;
- plugin and module-level health/readiness probes;
- explicitly named starter and stopper lifecycle hooks;
- stable scheduler-stamped package versions for the workload and its transitive
  project graph;
- canonical contribution identities and precise provenance for every v2
  entry, with declaration precision set by what the runtime can observe: the
  producer records the owning project file rather than inventing a symbol it
  cannot derive (see the migration entry in the golden);
- a native migration node linked to the feature scope without parallel metadata;
- an `sql` requirement that proves datasource, migration, and readiness are all
  present before the manifest is published.

`Application.build()` writes the deterministic manifest to
`.gen/schema/capabilities.json` and atomically publishes the disposable native
graph to `.gen/design/graph.json`. The sample test compares the capability
artifact byte-for-byte with its committed golden and verifies that the feature
and migration appear without a parallel capability-metadata writer or a feature-evidence
artifact. Framework tests separately retain compatibility coverage for the v1
evidence serializer under `protocols/`.

Source integrity is computed by the system indexing the selected revision. The
committed manifest therefore stays stable and does not contain the scheduler's
volatile source binding.

The workload's volatile deploy version is intentionally not emitted as a package
version. The scheduler separately stamps resolved, stable project versions plus
dependency manifest paths; the producer merges those manifests within the
stamped workspace root and records complete provenance. A complete legacy
scheduler stamp is treated as a no-publication compatibility state; malformed
or partially source-bound stamps fail closed.

Feature and design metadata remain build-only. The extension derives loader,
config, and startup activation from a normalized capability view, so changing
the native feature declaration cannot change runtime behavior.

Run the proof with:

```bash
./putnamiw lint,test,build --projects @example/14-capabilities
```

## What this sample proves

Capability publication is a build-phase step: `Application.build()` invalidates
the manifest first and writes it last, so no failed or partial build can leave
bytes that appear to describe it. Nothing here runs at startup, which is the
point — feature and design metadata are build-only and cannot change runtime
behavior.

Contract: [`@putnami/application`](../../framework/application/README.md) —
[application-lifecycle specification](../../framework/application/specs/application-lifecycle.json).
