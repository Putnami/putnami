// Registers @putnami/cloud's config-server sources in the framework config
// loader. Without this the loader reads only local files, CONFIG_DATA and
// env: the remote source behind CONFIG_SERVER_URL stays dormant and every
// value that lives only in the config service — `analytics.secret`
// included — is silently absent at runtime.
//
// The framework auto-activates `@putnami/cloud/runtime` when it can
// `require()` it, which covers dev and tests where node_modules exist. The
// deployed image is a bun-compiled binary where that dynamic require cannot
// resolve, so this static import is the bundler-proof path. The registrar
// dedupes by discoverer identity, so both firing together is safe.
import { register } from '@putnami/cloud/runtime';
import { registerConfigLoaderResetHook, registerSourceDiscoverer } from '@putnami/runtime';

register({ registerSourceDiscoverer, registerConfigLoaderResetHook });
