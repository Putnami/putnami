# ADR 0003: Deployers own runtime cost policy

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/infra` (`protocols/infra`) runtime block

## Context

Workloads know their capacity and execution requirements. Deployers know their
platform, cost model, workload classification, and operational safeguards. A
workload field for minimum residency or billing posture would let application
code impose fixed cost and change architecture without control-plane review.

## Decision

The runtime `scaling` block carries only workload-owned capacity ceilings: `max`
and `concurrency`. Minimum residency and billing or CPU-allocation posture are
deployer-owned and absent from the Go types and the JSON schema. The strict
readers reject `min`, `billing`, `requestBased`, `cpuIdle`, and
`cpuAlwaysAllocated`, and name the reason through `infra.RemovedRuntimeFields`
instead of a bare "unknown field".

Deployers choose residency and billing from workload nature and platform
constraints. A serverless deployer enforces scale-to-zero and request-based
execution without accepting an override from a workload manifest.

`ProtocolVersion` is 2. Every committed `infra/requirements.json` declares
`"protocolVersion": 2`. `LoadGeneratedPerProjectManifest` accepts a
generator-owned v1 manifest, normalizes it to v2 in memory with a warning, and
the next `putnami build` persists v2. Authored manifests and `runtime.json` get
no such bridge.

## Consequences

- Infra aggregation reports findings as warnings and still emits a manifest, so
  an unread declaration costs a workload its infrastructure silently. Every
  out-of-date diagnostic therefore carries `infra.MigrationGuide`.
- An authored `runtime.json` that still sets `scaling.min` must be edited by
  hand; no generator owns it.
- Deployers must supply or enforce minimum residency and billing posture.
