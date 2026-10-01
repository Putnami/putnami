# Site Content Bundles

putnami.dev can serve documentation produced in another repository — without that repository ever touching this site's build. Content travels as a **site content bundle** (`site-content-bundle/v1`): a digest-addressed `tar.gz` of raw markdown plus a `bundle.json` manifest declaring the site URL prefixes (mounts) the bundle owns.

Docs content is data, not code: bundles are versioned in the put registry on their own cadence, while the site binary is versioned with deploys.

## Baked consumption (v1)

The pipeline is deterministic by construction:

1. **Pin.** `sites/putnami.dev/content.lock.json` commits, per bundle, its registry package, a `channelHint`, the resolved `version`, and — the only field a build ever fetches by — the blob's sha256 `digest`. Version and channel are provenance for reviewers.
2. **Fetch by digest.** At generate time the site fetches each pinned blob by digest (with a digest-keyed local cache, so builds are offline-safe once fetched), verifies the digest, validates the manifest and payload (traversal, symlink, unlisted-file, and digest-mismatch rejection), and unpacks the files into `.gen/public/docs/<mount>`. The runtime navigation tree and docs loader scan `.gen/public/docs`, so mounted pages are served and navigable like any other doc. A mount dropped from the lock is pruned from `.gen` on the next generate (a recorded manifest of the previous run's mounts), so stale content is never re-captured into the build cache.
3. **Bump to advance.** `bun run content:bump` (in `sites/putnami.dev`) is the **only** place a channel is resolved: it asks the registry what `package@channelHint` currently points at, computes the digest from the fetched bytes, and rewrites the lock. The committed lock diff is the change detection and the review surface. A `putnami content bump` CLI verb is deferred until a second site consumes bundles.

The lock pins one bundle: the platform docs bundle, `cloud/doc-contents-platform`, which follows the `canary` channel. Its producing repository publishes it in its own `cloud` namespace and moves its `canary` channel when a commit changes the docs. The site only reads it. The running site's overlay polls the same `package@channelHint` pointer, so it picks up a new canary bundle without a deploy; `content:bump` pins it into the lock.

The build never resolves a channel and never checks a URL for "newness" — that would make the generate cache key lie about its inputs. The lock file participates in the generate cache key, so the same lock reproduces byte-identical docs and a lock bump is what produces a new build.

## Mounts partition the URL space

A bundle declares the URL prefixes it owns, e.g. `/docs/08-cloud`. Bundle-provided sections use the same `NN-` ordering prefix as site-local sections, so navigation ordering just works. Equal, parent, or child prefix overlap — between two bundles, or between a bundle and content the site materializes itself — is a hard build error, never a silent overwrite.

## Failure policy

Baked content is the always-available fallback; the registry is never on the serving path.

- A pinned bundle that cannot be fetched or fails verification **fails the build in CI/deploy** and is **skipped with a warning in local dev**, so an offline laptop still builds the site.
- A bundle with a newer `formatVersion` major than the site understands is always skipped in favor of baked content — updating the site is the fix, and forcing a deploy is exactly what the baked path is for.

## Known limitations

These apply to every pinned bundle, including the platform bundle:

- **Static prerender is best-effort for bundled pages.** SSG static-path enumeration and bundle materialization both run in the concurrent generate phase with no ordering edge between them, so a mounted page may not be prerendered to static HTML. It is still served at runtime (the docs loader and nav scan `.gen/public/docs`), so this is a performance/first-paint concern, not a correctness one. Making it first-class needs a build-ordering guarantee that materialization completes before enumeration.
- **Bundled pages are in the search index, one phase later.** The search index is built from the materialized `.gen/public/docs` tree in the `postGenerate` phase — the ordering barrier after every `generate()` producer (asset copies, bundles, the support catalog) has written its pages — so bundled and copied docs are searchable alongside site-local ones. The standalone `scripts/build-search-index.ts` CLI still defaults to the site-local `doc/` tree; pass `--docs-root .gen/public/docs` to reproduce the build's index.
