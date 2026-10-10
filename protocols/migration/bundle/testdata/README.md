# Packing fixture

`valid/` is one `migration-bundle.v1` directory: a single SQL operation with an
up and a down payload. It holds no secret and no personal data. The packing
tests pin its bytes, so do not edit it: a changed byte moves every digest below.

| File | SHA-256 |
| --- | --- |
| `payload/sql/platform_migrations/publish-v2/001_fixture.up.sql` | `81e19b2a77797020f0c77525bfb7cd044d505232f6f8c0c67b9f9153c06a010c` |
| `payload/sql/platform_migrations/publish-v2/001_fixture.down.sql` | `26129ebb9c1fc9954a02d2fadbc8ee3d0560fd9cddf8c07b1b0bcb471fe2fb2c` |

The bundle digest is
`4f12fb7c002958eebb90da5d38679d8b794e2522d6d89202f399b4ef955c1955`.
`ComputeBundleDigest` computes it over the compact canonical subset
(`protocol`, `appName`, normalized `operations`), not over the bytes of the
pretty-printed `bundle.json`.

`bundle.Pack` packs the directory to the tar blob
`sha256:0c5a4a88a758035dcb5b46bc35b74847e8844bbe6156a08d68623e779ba787a4`.

The protocol's own corpus under `../../fixtures/invalid/` covers semantic
rejection, such as a bad payload hash or an unknown protocol.
