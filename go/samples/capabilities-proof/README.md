# capabilities-proof

A Go proof workload for stable capability provenance and the native design
graph. It retains the established `capabilities/source-bound-manifest` feature
ID and declares it once on the application
and composes every v2 contribution collection beneath that scope:

| Kind                   | Source in this workload                                      |
| ---------------------- | ------------------------------------------------------------ |
| `configDefinitions`    | `proof` config, including one sensitive field                |
| `schemas`              | route and generated OpenAPI inventory                        |
| `discoverers`          | config and typed migration-source discovery                  |
| `migrations`           | `iamSource` (namespace `iam`, bound to datasource `default`) |
| `infraRequirements`    | typed infra sidecars, including the database `iamSource` declares |
| `healthContributors`   | `diskSpace` (health) and `primaryDatabase` (readiness)       |
| `lifecycleHooks`       | `connectionPool` (starter + stopper)                         |
| `packageVersions`      | scheduler-stamped self + transitive workspace package graph  |
| `requiredCapabilities` | `sql` requires datasource + migration + readiness            |

OpenAPI entries are only emitted when their concrete artifacts exist; their
artifact provenance is derived directly from `CapabilitySchemas()` rather than
repeated in feature metadata. The migration and database schema are projected
into `.gen/design/graph.json` from `MigrationSources()` and
`InfraDatabases()`. Dependency manifests are discovered from the
scheduler-stamped transitive project graph and merged under the stamped
workspace root; they do not depend on runtime plugin activation.

`migrationPlugin.RequiredCapabilities()` makes the proof fail closed if its
datasource, migration, or readiness contribution is removed.

## How it emits

`BuildApp()` assembles the app. The built-in capabilities describer runs during
`app.Describe`, writes the manifest to `.gen/schema/capabilities.json` (the
`capabilities.EmitDir`), and the native design describer atomically writes the
disposable graph to `.gen/design/graph.json`. No parallel capability-metadata
writer or generated feature-evidence artifact is part of this routine authoring path.

`app_test.go` drives the real describe pipeline (`BuildApp().Describe`) with a
controlled `.gen/version.json`, pins the capability bytes against
`testdata/capabilities.golden.json`, and asserts the manifest and native graph
are deterministic and pass strict protocol validation. Source integrity is an
index-time concern, so the committed manifest does not contain the scheduler's
volatile source binding. Regenerate the golden with
`PUTNAMI_UPDATE_GOLDEN=1 ./putnamiw test --projects go.putnami.dev/examples/capabilities-proof`.

This proof is a library, not a runnable binary, on purpose: the deterministic
proof lives in the test, where the project identity is controlled explicitly.
It is executable evidence that the stable app and config packages participate
in describe mode without starting runtime plugins or reaching external systems.
