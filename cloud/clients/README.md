# Generated service clients

Each directory here holds the generated Go client of one Putnami Cloud service
that the extension binary calls. Each one is its own Putnami project,
`go.putnami.dev/cloud/clients/<service>`, with its Go package under
`<service>/go`. `./putnamiw clientgen` generates `<service>/go/client.gen.go`
from `<service>/schema/openapi.json`. Never edit `client.gen.go` by hand;
regenerate it.

The services are auth-server, cache-server, config-api, control-api, data-api,
db-gateway, delivery-api, distribution-api, identity-api, observability-api,
oci-server, put-server, runtime-api and source-api.

## Schema source

| Field | Value |
| --- | --- |
| Put package | `cloud/doc-contents-cloud-cli-schema` |
| Channel | `canary` |
| Version | The canary build published on 2026-10-11, archive digest `sha256:2e51cc3be3ad5cee1bbfead0d9cdbf7b697088d6a861af181c001cacf73026f8` |
| File layout | `cloud-cli-schema/services/<service>/openapi.json` |

Each `<service>/schema/openapi.json` is a byte-for-byte copy of the package
file, with one exception. The `info.description` of control-api, data-api,
delivery-api and runtime-api drops the issue numbers, and control-api also
drops a source path. The public-cut gate rejects both. Apply the same edit
after each update until the providers drop them. No operation or schema
differs from the package.

## Update the schemas

1. Fetch a newer version of `cloud/doc-contents-cloud-cli-schema`.
2. Copy each `services/<service>/openapi.json` over
   `<service>/schema/openapi.json`, then remove the issue numbers and source
   paths from `info.description`.
3. Run `./putnamiw clientgen --projects <the client projects>`, for example
   `--projects go.putnami.dev/cloud/clients/put-server`.
4. Adapt the call sites in `../extension/internal` to the regenerated clients.

Calls with no declared contract stay hand-written. See
[Service clients](../extension/README.md#service-clients) for the two inventories that
list them.
