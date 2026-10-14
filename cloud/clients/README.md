# Generated service clients

Each directory here holds the generated Go client of one Putnami Cloud service
that the extension binary calls. Each one is its own Putnami project,
`go.putnami.dev/cloud/clients/<service>`, with its Go package under
`<service>/go`. `./putnamiw clientgen` generates `<service>/go/client.gen.go`
from `<service>/schema/openapi.json`, with the targets, package and client
names in `<service>/clientgen.json`. Never edit `client.gen.go` by hand;
regenerate it.

`clientgen.json` is the generation contract a provider build writes to
`.gen/clientgen/config.json`. A slice has no app to build, so it commits the
contract at its root. Do not commit it under `.gen/clientgen`: the Go
`build-describe` empties that directory before it runs.

The services are auth-server, cache-server, config-api, control-api, data-api,
db-gateway, delivery-api, distribution-api, identity-api, observability-api,
oci-server, put-server, runtime-api and source-api.

## Schema source

| Field | Value |
| --- | --- |
| Put package | `cloud/doc-contents-cloud-cli-schema` |
| Channel | `canary` |
| Version | `0.0.0-20261014111756-10ec53b5f`, published on 2026-10-14, archive digest `sha256:81201d632697918965f36aadd602534526f2b44d74f886c5331c4725f8dbcccb` |
| File layout | `docs/cloud-cli-schema/services/<service>/openapi.json` |

Each `<service>/schema/openapi.json` is a byte-for-byte copy of the package
file.

## Update the schemas

1. Fetch a newer version of `cloud/doc-contents-cloud-cli-schema`.
2. Copy each `services/<service>/openapi.json` over
   `<service>/schema/openapi.json`.
3. Run `./putnamiw clientgen --projects <the client projects>`, for example
   `--projects go.putnami.dev/cloud/clients/put-server`.
4. Adapt the call sites in `../extension/internal` to the regenerated clients.

Calls with no declared contract stay hand-written. See
[Service clients](../extension/README.md#service-clients) for the two inventories that
list them.
