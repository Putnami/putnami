// Remote config + secrets sources (priority 50 / 55) and their discover*
// helpers.
export {
  RemoteConfigSource,
  RemoteConfigError,
  discoverRemoteSource,
  configServerURLIsResolveEndpoint,
  resetRemoteConfigSourceCacheForTest,
} from './remote-config-source';
export type { ResolveResponse } from './remote-config-source';
export {
  RemoteSecretsSource,
  discoverRemoteSecretsSource,
  resetRemoteSecretsSourceCacheForTest,
} from './remote-secrets-source';

// Identity / token sources: the GCP-metadata token source and the
// CONFIG_SERVER_TOKEN env-name precedence.
export {
  CONFIG_SERVER_TOKEN_ENV_NAMES,
  discoverTokenSource,
  EnvVarTokenSource,
  GcpMetadataTokenSource,
  parseJWTExpiry,
  resetTokenSourceCacheForTest,
} from './token-source';
export type { TokenSource } from './token-source';

// Strict managed Event Server destination. The framework owns provider-neutral
// request/retry semantics; Cloud owns generation pinning and Google identity.
export {
  cachedEventServerTokenSource,
  eventServerDestination,
  GOOGLE_METADATA_IDENTITY_ENDPOINT,
  googleEventServerTokenSource,
} from './event-server-destination';
export type {
  EventServerCredentialCacheOptions,
  EventServerDestinationOptions,
  EventServerRouteDescriptor,
  GoogleEventServerTokenSourceOptions,
  ManagedEventServerBinding,
  ManagedEventServerEventsConfig,
} from './event-server-destination';

// Registration entry point for the framework config loader.
export { cloudConfigLoaderResetHooks, cloudSourceDiscoverers, register } from './register';
export type { CloudRuntimeRegistrar, SourceDiscoverer } from './register';
