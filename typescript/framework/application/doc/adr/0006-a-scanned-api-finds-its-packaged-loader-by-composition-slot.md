# ADR 0006 — A generated loader family keys its instances by composition slot

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`), `@putnami/events`, `@putnami/storage`

## Context

A plugin that scans a folder generates a loader module at build time. The
packaged serve entrypoint cannot import a path known only at runtime, so it
statically imports every generated loader and registers it under the export key
the build reported; each plugin looks its loader up by key before falling back
to a dynamic import. The build merges plugin exports last-wins, so two plugins
of one family sharing a key lose one loader in the package, while `putnami
serve` still shows it.

A key derived from the scan path cannot work in the package. There,
`import.meta.dir` names the bundle (`/$bunfs/root` in a compiled binary, the
output directory in a `bun build` bundle), so an explicit `scanPath` points into
the bundle and the default `api()` finds no folder at all. The build and the
binary would compute different keys.

`storage-loader`, `sql-loader`, `config-loader` and `react-loader` are
project-wide: every instance emits the same key, path and bytes, so the
last-wins merge loses nothing and they stay unslotted.

## Decision

1. The key is the plugin's slot among the application's plugins of its family
   that own a loader, in module-tree registration order: `<family>-loader` for
   slot 0, `<family>-<slot>-loader` otherwise. `src/bundled/loader-slot.ts`
   owns the rule (`generatedLoaderKey`, `generatedLoaderPlugins`,
   `generatedLoaderSlot`); `api()`, `static()` and `events()` use it and keep no
   copy. The build and the binary run the same composition, so they compute the
   same slot.
2. Loader ownership reads configuration only, never the filesystem, because the
   packaged binary has no scan folder:
   - `api()` and `events()`: the plugin names a `scanPath` or leaves `autoScan`
     on.
   - `static()`: every StaticPlugin without `skipLoading`. Its `scanPath` is a
     filesystem fact (`staticFiles()` finds `public/` at build time and nothing
     in the package), so it does not decide ownership.
   A plugin that owns no loader takes no slot and resolves no loader.
3. A StaticPlugin at slot `n > 0` stages its files under
   `.gen/public/.static-<n>/` and writes `.gen/src/static/.static-<n>.gen.ts`.
   The slot-0 scan never matches a hidden entry. Every file still ships inside
   the one `.gen/public` that packaging copies and `publicFolder()` serves.
4. Plugins generate concurrently, so the first `events()` plugin with a scan
   folder scans every sibling's folder and writes their union as the single
   infra sidecar (`.gen/infra/events.json`). A topic two plugins subscribe with
   different deliveries fails the build.
5. Slotted keys are generated `source` discoverers in the capability manifest.
   The TypeScript producer and the TypeScript extension
   (`typescript/extension/internal/build/capabilities.go`) keep route schemas
   for the first-slot keys only and treat every other `-loader` as a source
   discoverer. Adding slotted keys to the route list (by prefix match, for
   example) would make an older extension refuse a newer framework's manifest
   ("conflicts with schema identity"). An older framework emits only first-slot
   keys, so a newer extension builds it unchanged.
   `TestReconcileCapabilityManifestV2_SlottedLoadersAreSourceDiscoverersAndActivate`
   and its producer twin pin this.

## Consequences

- A workload with one plugin per family exports the same keys, paths and loader
  bytes as a single-key design.
- A composition that differs between build and runtime (an `api()` added only
  when an environment variable is set) shifts every slot after it. Compose the
  same tree in both phases.
- The manifest names a slot, not a folder; the folder is readable from the
  loader's path.
- A stale `.gen/public/.static-<n>/` from an earlier build is inert: no loader
  references it and slot 0 does not scan it.
