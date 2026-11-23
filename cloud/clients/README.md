# Generated service clients

Each directory here holds the generated Go client of one Putnami Cloud service
that the extension binary calls. Each one is its own Putnami project,
`go.putnami.dev/cloud/clients/<service>`, with its Go package under
`<service>/go`. `./putnamiw client-generate` generates
`<service>/go/client.gen.go` from `<service>/schema/openapi.json`, with the
package and client names in `<service>/clientgen.json`. The extension's build
runs it first, so you rarely run it yourself. The extension's
`bin/prepare-runtime` generates the same clients from the same files into a
temporary overlay, because the CLI builds that runtime before any job runs. Git
ignores `client.gen.go`: never edit or commit it.

`clientgen.json` declares a client that lives inside its provider's own Go
module. Its `go.output` names the package directory, here `go`.

The services are agency-api, auth-server, cache-server, config-api, control-api, data-api,
db-gateway, delivery-api, distribution-api, identity-api, observability-api,
oci-server, put-server, runtime-api and source-api.

## Schema source

| Field | Value |
| --- | --- |
| Put package | `cloud/doc-contents-cloud-cli-schema` |
| Channel | `canary` |
| Version | `0.0.0-20261102145238-eda90513c`, published on 2026-11-02, archive digest `sha256:dab1f1b3075d46e412de527001649b4e46a7f8374e57764ae532bf5216df1204` |
| File layout | `docs/cloud-cli-schema/services/<service>/openapi.json` |

Each `<service>/schema/openapi.json` is a byte-for-byte copy of the package
file, except the ones named below. `delivery-api` comes from the later version
`0.0.0-20261123111359-587eb29f5`, published on 2026-11-23, archive digest
`sha256:7b962ef088c65103e2dc061272450a14d5d3a109f1da4520b5e6eb8111cd65f8`. It
carries the CI run of a records submission (`ciSystem`, `runId`, `runAttempt`
and `runUrl` of `SubmitRequest`), which `putnami cloud evidence-report` sends,
and its read (`CIRunResponse.ci`). It also carries the merge queue setting and entries
(`getV1_Workspaces_Workspace_Ci_Queue`, `putV1_Workspaces_Workspace_Ci_Queue`,
`getV1_Workspaces_Workspace_Ci_Queue_Entries`), the test cases of a tests
report (`cases` and `omittedCases` of `ProjectTests`), which
`putnami cloud evidence-report` sends, the level 3 preview a report batch answers
(`ReportBatchResponse.level3Preview`) and the provider of a CI run
(`CIRunResponse.provider`). Its queue descriptions say the entries of a deeper
queue build side by side. `source-api` comes from the later version
`0.0.0-20261115110438-6cf844fba`, published on 2026-11-15, archive digest
`sha256:7edc53105fab142c91a4c50d2b3aef6df3d8c6121a305c9013a0abf45a40bbf9`. It
adds the collaboration routes under `source/proposals`, `source/tasks` and
`source/collaboration/capabilities`, which `@putnami/cloud` calls to serve the
tasks and proposals contracts.

Two schemas come from the putnami-cloud repository instead of the package, and
`observability-api` adds one path from it, as the next paragraphs say.
`agency-api/schema/openapi.json` is a byte-for-byte copy of
`agency/workloads/agency-api/schema/openapi.json` at commit
`e5de36e8d8249a8c54f45fddb5195fe255cb713e` of `main`,
which serves the memory, missions (v1 and v2 runs), build start and audit
attestation routes `@putnami/cloud` calls. `config-api/schema/openapi.json` is a
byte-for-byte copy of `config/workloads/config-api/schema/openapi.json` at
commit `bdecdb94ef12611b2440bc9513a0562543b479e6` of `main`, which carries the
agent trigger and organization model credential routes of
`putnami agency trigger` and `putnami agency model-key`, and the parent policy
and expiry of the telemetry settings `putnami cloud telemetry` writes. Refresh either one
the same way, from the service's committed `schema/openapi.json`.

`observability-api/schema/openapi.json` is the package file above plus one
path, `/v1/workspaces/{workspace}/traces/{traceId}`
(`getV1_Workspaces_Workspace_Traces_TraceId`), copied byte-for-byte from
`observability/workloads/observability-api/schema/openapi.json` at
putnami-cloud commit `d0b960608af2cd8406741f0bb1ef54dc4b962883` of `main`. The
trace read references only `TraceSummary` and `Span`, which match the package
file. `putnami cloud traces --trace` calls it. The rest of the file stays at the
package version on purpose: the provider's current schema also gives
`logs/tail` a WebSocket transport, which changes how the generated tail client
connects, and that move belongs to its own change. The next refresh from a
package version whose selection carries the trace read replaces this copy whole.

## Update the schemas

1. Fetch a newer version of `cloud/doc-contents-cloud-cli-schema`.
   Read the version's manifest at
   `https://put.putnami.dev/cloud/doc-contents-cloud-cli-schema/versions/<version>/manifest`
   with the bearer from `putnami cloud token put`. Its `payload.artifact.blob`
   is the archive digest. Download the archive from `.../blobs/<digest>`, check
   its SHA-256 equals that digest, and unpack it.
2. Copy each `services/<service>/openapi.json` over
   `<service>/schema/openapi.json`.
3. Run `./putnamiw client-generate --projects <the client projects>`, for
   example `--projects go.putnami.dev/cloud/clients/put-server`.
4. Adapt the call sites in `../extension/internal` to the regenerated clients.

Calls with no declared contract stay hand-written. See
[Service clients](../extension/README.md#service-clients) for the two inventories that
list them.
