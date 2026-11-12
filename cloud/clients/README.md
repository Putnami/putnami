# Generated service clients

Each directory here holds the generated Go client of one Putnami Cloud service
that the extension binary calls. Each one is its own Putnami project,
`go.putnami.dev/cloud/clients/<service>`, with its Go package under
`<service>/go`. `./putnamiw client-generate` generates
`<service>/go/client.gen.go` from `<service>/schema/openapi.json`, with the
package and client names in `<service>/clientgen.json`. The extension's build
runs it first, so you rarely run it yourself. Git ignores `client.gen.go`:
never edit or commit it.

`clientgen.json` declares a client that lives inside its provider's own Go
module. Its `go.output` names the package directory, here `go`.

The services are auth-server, cache-server, config-api, control-api, data-api,
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
file. `delivery-api` comes from the later version
`0.0.0-20261110144910-13ece114a`, published on 2026-11-10, archive digest
`sha256:082572ded5b2acb20382e546b27da1694abc630ff83b6530988a5568052a3fed`. It
adds the merge queue setting (`getV1_Workspaces_Workspace_Ci_Queue`,
`putV1_Workspaces_Workspace_Ci_Queue`).

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
