import type { ConfigSource } from '@putnami/runtime';
import { discoverRemoteSource, resetRemoteConfigSourceCacheForTest } from './remote-config-source';
import { discoverRemoteSecretsSource, resetRemoteSecretsSourceCacheForTest } from './remote-secrets-source';
import { resetTokenSourceCacheForTest } from './token-source';

/**
 * Discovers a cloud config source from the environment. Mirrors the framework
 * loader's own discoverer shape: it inspects `CONFIG_SERVER_URL` (plus token /
 * GCP signals) at call time and returns a source, or `undefined` when the
 * environment doesn't enable it. Re-reads the environment on every call, so
 * registration is independent of when the loader actually runs.
 */
export type SourceDiscoverer = () => ConfigSource | undefined;

/**
 * The registrar the framework config loader hands to {@link register}. The
 * framework owns the registry: `getDefaultSources()` drains the registered
 * discoverers after its built-in file/env sources, and `resetConfigLoader()`
 * invokes the reset hooks.
 */
export interface CloudRuntimeRegistrar {
  registerSourceDiscoverer(discoverer: SourceDiscoverer): void;
  registerConfigLoaderResetHook?(reset: () => void): void;
}

/**
 * Config-source discoverers this package contributes, in priority order:
 *
 *   - {@link discoverRemoteSource}        — `RemoteConfigSource`  (priority 50, `/api/configs/resolve`)
 *   - {@link discoverRemoteSecretsSource} — `RemoteSecretsSource` (priority 55, `/api/secrets/resolve`)
 *
 * Priority lives on the source objects, never on the loader, so ordering
 * relative to the local `.secrets` (35) and `CONFIG_DATA` (60) sources is
 * fixed by the sources themselves.
 */
export const cloudSourceDiscoverers: readonly SourceDiscoverer[] = [discoverRemoteSource, discoverRemoteSecretsSource];

/**
 * Cache-reset hooks the framework's `resetConfigLoader()` invokes so test
 * isolation clears this package's process-wide caches.
 */
export const cloudConfigLoaderResetHooks: ReadonlyArray<() => void> = [
  resetRemoteConfigSourceCacheForTest,
  resetRemoteSecretsSourceCacheForTest,
  resetTokenSourceCacheForTest,
];

/**
 * Registers this package's config/secrets sources with the framework config
 * loader. The framework calls it when it can resolve `@putnami/cloud/runtime`;
 * a bundled binary activates it with a static import instead.
 *
 * The registrar is dependency-injected. Activation is a one-line, additive
 * side effect:
 *
 * ```ts
 * import { registerSourceDiscoverer, registerConfigLoaderResetHook } from '@putnami/runtime';
 * import { register } from '@putnami/cloud/runtime';
 * register({ registerSourceDiscoverer, registerConfigLoaderResetHook });
 * ```
 */
export function register(registrar: CloudRuntimeRegistrar): void {
  for (const discoverer of cloudSourceDiscoverers) {
    registrar.registerSourceDiscoverer(discoverer);
  }
  if (registrar.registerConfigLoaderResetHook) {
    for (const reset of cloudConfigLoaderResetHooks) {
      registrar.registerConfigLoaderResetHook(reset);
    }
  }
}
