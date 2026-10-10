# Distribution CLI

This package holds the Distribution commands of the `@putnami/cloud`
extension: registry credentials, namespace bindings and grants, public mirror
targets, the release-set provider, OCI image operations, and archive and
site-content publishing. A release set is the immutable record of what one
publishing run released. The import path is
`go.putnami.dev/cloud/extension/internal/distributioncli`, package
`distributioncli`.

The package has no entrypoint of its own. The aggregator in
[`internal/cloudcli`](../cloudcli) imports it, and `internal/cloudcli/cli.go`
maps each command name to one exported function here.
[`putnami.extension.json`](../../putnami.extension.json) declares the commands
and the lifecycle tasks that run them.

## Commands

- `putnami cloud registries [setup|status [<registry>]|logout]`: writes the per-registry endpoint and token recipe to `~/.putnami/registries.json`, checks each registry and the stored key auth-server holds for it, and revokes the keys.
- `putnami cloud registry-token --for npm|go|oci|put` (or `--host <host>`): prints one short-lived `aud=distribution` bearer for the signed-in user and one registry kind. Native publishers call it as their credential seam.
- `putnami cloud packages status [<protocol>]`: checks that the linked workspace binds the npm, go, oci and put namespaces this checkout needs. It reads the bindings once; `putnami.ci.json` decides which protocols are needed. When oci is needed and not bound, it also reads the package shares the workspace received: one active OCI publisher share makes oci `ok`. If that read fails, oci stays `degraded` and the detail says why.
- `putnami cloud packages namespaces|grants|mirrors ...`: activates and lists namespace bindings, creates, lists, and revokes owner grants, and adds, lists, removes, and rotates public mirror targets (`--ecosystem npm|oci|archive`; an `archive` target also takes `--package <namespace>/<name>` and copies that package to GitHub Release assets).
- `putnami cloud channels set <channel> --from <channel|rs_id>`: points a channel at a release set that already exists, for a promotion or a rollback. It sends one `channel-set` request to the release-set provider, with `--expected <rs_id>` as an optional compare-and-swap.
- `putnami cloud channels status [<channel>] [--wait <duration>]`: shows the head of a channel and the generation each registry applied. Without a name it shows every channel `putnami.ci.json` declares. A registry behind the head is degraded. `--wait` polls until every registry applied the head, and the channel is failing when one is still behind at the deadline.
- `putnami cloud release-set resolve|release|channel-set|channel-status --request-file <path>`: the `distribution/release-set/v2` provider seam. It reads one request file and prints one validated protocol response.
- `putnami cloud packages copy|retag|revert`: server-side image copy, retag, and tag revert on the Putnami OCI registry.
- `putnami cloud packages publish`: publishes a project's archive packages to put-server.
- `DeriveSiteContentSections`, `AssembleSiteContentSection` and `PublishSiteContentBundles` derive, package and publish site-content bundles. The direct operator path, `putnami operator publish-doc`, is not a command of this CLI.
- The `publish`, `package`, and `validate` lifecycle tasks call `PublishArchivesWithResult`, `PublishSiteContentMembers`, `PackageSiteContent`, and `ValidateSiteContentSources` for each selected project.
- Under the engine's `publication-v1` capability (`PUTNAMI_PUBLICATION_OUTBOX` set), `PublishArchivesWithResult`, `PublishSiteContentMembers`, `PackConfigMember` and `PackDeploymentMember` pack the one planned member into the publication outbox instead of publishing it. The packed manifest and blobs are the bytes the direct path uploads, so the artifact digest is the same. Each member passes the engine's put-write/v1 upload check (`putpublish.Check`) before it is committed to the outbox.

The aggregator also calls helpers from this package in its own commands:
`WriteRegistryTokenRecipes` from `cloud login`, `cloud setup`, and
`cloud install`, `TeardownRegistries` from `cloud logout`,
`ResolvePutPublicationBearer` from `db migrations publish`,
`PublishConfigMember` and `PackConfigMember` from `config publish`,
`PublishDeploymentMember` and `PackDeploymentMember` from the deployment
member publish step, the developer-machine read credential from
`credential-provider`, and `RegistriesStatusNode`, `ChannelsStatusNode` and
`PackagesStatusNode` from `cloud status`. Each status node is the node its own
`status` command prints.

## Layout

The package has no sub-packages. Files group by concern:

- **Package doc:** `distributioncli.go`.
- **Registry credentials:** `registries.go` (the four registry kinds and endpoints), `registries_setup.go`, `registries_token.go`, `registries_writers.go` (native credential formats), `registries_apikey.go` (API key reads and revocation through auth-server's generated client), and `registries_credential.go` (the developer-machine read bearer of `credential-provider`, cached in `~/.putnami/registries.json` under a key no host can take).
- **Owner administration:** `distribution_admin.go` (bindings and grants), `distribution_mirrors.go` (mirror targets; credentials arrive on standard input, never as a flag), and `distribution_clients.go` (generated client bindings).
- **Release sets:** `release_set.go`, `archive_release_plan.go`, and `channels.go` (`channels set`).
- **Status:** `registries_status.go`, `channels_status.go` and `packages_status.go` build the status node of `registries status`, `channels status` and `packages status`. Each has a pure `...StatusNodeFrom` fold that its tests drive.
- **Publishing:** `publish_archives.go`, `publish_migration.go` (Config member publication), `publish_deployment_member.go` (deployment member publication), `publish_site_content.go`, `publish_site_content_member.go`, `put_publish_client.go` (blob upload, atomic publish, channel move and manifest readback through put-server's generated client), `publication_outbox.go` (publication-v1 outbox packing), and `validate_site_content.go`.
- **OCI:** `oci.go` probes the registry's `/v2/_putnami/capabilities`. `copy` and `retag` fall back to the standard distribution API when the registry does not advertise a fast path. `revert` has no fallback, because it needs the registry's recorded tag-move history.
- **Compatibility:** `publishprovider.go` keeps the old `publish-provider` command, which the extension manifest no longer declares.

## Dependencies

It calls Cloud services through their generated Go clients:
[`cloud/clients/put-server`](../../../clients/put-server),
[`cloud/clients/oci-server`](../../../clients/oci-server),
[`cloud/clients/distribution-api`](../../../clients/distribution-api), and auth-server's
client through [`internal/clicore`](../clicore). One call is still
hand-written HTTP: the `_putnami` fast paths of oci-server in `oci.go`
(`copy`, `tag-digest` and `revert`). oci-server's contract does not yet
describe them fully, so
[`clientgen.framework.json`](../../clientgen.framework.json) claims that call
site as `pending-provider-contract`. The standard OCI distribution fallback is
listed in [`clientgen.external.json`](../../clientgen.external.json).

Shared IO, auth, and flag handling come from `internal/clicore`.

## Consumers

- [`internal/cloudcli`](../cloudcli), the aggregator, is the only package that
  imports it.

## Test

```sh
./putnamiw test --projects @putnami/cloud-extension
./putnamiw lint --projects @putnami/cloud-extension
```

The tests use `httptest` servers and temporary home directories in place of
real services, so they need no database and no network.

## Read next

- [`@putnami/cloud` CLI source layout](../../README.md#source-layout).
- [Service clients](../../README.md#service-clients).
