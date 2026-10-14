# ADR 0004: A secret is named by its config key

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/infra` (`protocols/infra`) `secrets` lists

## Context

A sensitive config field `apiKey` in section `crm` has two names. The
application reads its secret at the config key `crm.apiKey`, and
`putnami cloud secrets set` and the deploy-time secret check use that key. The
`secrets` list of `infra/requirements.json` names it `crm.api_key`, because the
resource-name grammar accepts only lowercase letters. An operator who copies the
requirement name sets a secret the application never reads (#253).

## Decision

A secret requirement is named by the config key the application reads it at.
The secret-name grammar is the resource-name grammar with uppercase letters:
`^[A-Za-z0-9][A-Za-z0-9_./-]{0,63}$`. It applies to `secrets` in per-project and
aggregated manifests and to `ignore.secrets` in overrides. Every other resource
name stays lowercase. Secret names are case-sensitive: `crm.apiKey` and
`crm.apikey` are two secrets.

`ProtocolVersion` stays 2: a new version would make every older reader refuse
every manifest, while keeping it makes an older reader refuse only a camelCase
secret name. Because of that refusal, the change ships in three steps:

1. The protocol accepts camelCase secret names. Emitters keep their current
   names.
2. Putnami Cloud, which validates every deployment with this package, adopts
   that version.
3. The Go and TypeScript emitters write the config key. `infra.SecretName`, the
   TypeScript `canonicalSecretName` camelCase split, and the `secrets set`
   refusal of a requirement name are deleted.

## Consequences

- Until step 3, `infra/requirements.json` still lists the lowercase name, and
  `secrets set` keeps refusing it and naming the config key.
- Between steps 1 and 2, local validation, the published `putnami-infra.json`
  schema and editors accept a hand-written `crm.apiKey`, while Putnami Cloud
  still refuses a deployment that lists it.
- Step 3 changes every committed manifest that lists a camelCase or
  env-bound secret, and waits for step 2: a deployer on an older version of
  this package refuses those manifests.
