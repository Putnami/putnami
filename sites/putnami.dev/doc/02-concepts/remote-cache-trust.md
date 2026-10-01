# Trust remote build cache entries

Remote caching moves build results between developers and CI, but those runs do
not have the same authority. Putnami keeps the speed benefit while making the
trust boundary explicit: the cache provider classifies every provenance-aware
entry as `trusted` or `hint`, and the CLI decides whether that class may satisfy
the current run.

Use `--cache-trust` on build, test, lint, package, or another job command:

```bash
# Authoritative: only provider-verified trusted entries may pass a job
putnami lint,test,build --cache-trust=ci

# Developer workflow: accept all remote entries
putnami build --cache-trust=any

# Keep the local store but do not contact the remote cache
putnami build --cache-trust=none
```

| Policy | What may satisfy a job | Default |
|---|---|---|
| `ci` | Only `trusted` entries | CI |
| `any` | Trusted entries, developer hints, and legacy entries | Local development |
| `none` | No remote entry; local cache entries still apply | — |

The key guarantee is negative: a hint never marks a job green under `ci`.
Putnami may still download and digest-verify the hint's blobs into the local CAS,
so the real execution can reuse those bytes. It does not install the hint's
result as a local action-cache entry, and the job re-executes.

Providers that predate provenance return no channel. They keep their historical
behavior under `any`; under `ci`, Putnami treats them as hints and re-executes.

Remote successful-run markers do not have provenance yet, so they can influence
bare `main` or `master` project selection only under `any`. With `ci`, Putnami
uses a local successful-run marker when available and otherwise selects every
project.

## Objects follow the same rule

A provider may also serve an **object cache**: small payloads a job reads
directly, such as a compiler's own build-cache entries. Those objects carry the
same `trusted` / `hint` channel, stamped by the provider from its credential,
and the run's policy filters them identically — under `ci` a job asks for
trusted objects only, and a hint object is simply not there. Putnami exports the
resolved policy to every job (`PUTNAMI_CACHE_TRUST`) so the filter is the run's
decision, not each tool's.

The guarantee is the same negative one: work a developer's machine produced can
warm a CI run's bytes, but it can never be what a CI run trusts.

## Configure the policy

Precedence is:

1. `--cache-trust`
2. `PUTNAMI_CACHE_TRUST`
3. `options.<command>.cache-trust` in `putnami.workspace.json`
4. `ci` when `CI` is set, otherwise `any`

For example, make builds authoritative without changing local test behavior:

```json
{
  "options": {
    "build": { "cache-trust": "ci" },
    "test": { "cache-trust": "any" }
  }
}
```

A multi-command invocation uses the strictest configured command policy because
it shares one provider session. Release paths (`publish`, `tag`, and
`version tag`) force at least `ci`; `--cache-trust=none` remains a stricter
remote opt-out.
