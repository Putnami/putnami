import { Config, Default, Int, MapOf, Optional, Sensitive, configToken } from '@putnami/runtime';
import { assertHttpUrl } from '../runtime/url';

/**
 * Configuration for service client discovery and defaults.
 *
 * Service URLs are resolved from config — in local dev they point to localhost,
 * in production to the real service endpoints. No service registry needed.
 *
 * @example YAML config (conf/.env.local.yaml)
 * ```yaml
 * client:
 *   services:
 *     users-api: http://localhost:3000
 *     orders-api: http://localhost:3001
 *   timeoutMs: 5000
 * ```
 *
 * @example Environment variables
 * ```bash
 * CLIENT_TIMEOUT_MS=10000
 * ```
 */
export const ClientDefaultsConfig = Config('client', {
  /** Default request timeout in milliseconds */
  timeoutMs: Default(Int, 30_000),
  /** Default maximum retry attempts */
  maxRetries: Default(Int, 3),
  /** Default base delay for retry backoff in ms */
  retryBaseDelayMs: Default(Int, 200),
  /** Default maximum delay for retry backoff in ms */
  retryMaxDelayMs: Default(Int, 5000),
  /**
   * Default maximum response body size in bytes. `0` selects the 32 MiB default
   * (Go parity — mirrors the Go client's `MaxResponseSize`). Responses larger
   * than the cap fail with `ClientResponseSizeError` instead of buffering
   * unbounded.
   */
  maxResponseSize: Default(Int, 0),
});

/**
 * Deployment-owned bindings for generated first-party clients. The framework
 * resolves this block through normal config/DI; generated code contains only
 * service/profile names and never these values.
 */
export const GeneratedClientsConfig = Config('clients', {
  clientId: Default(String, ''),
  services: Default(
    MapOf(String, {
      url: String,
      allowInsecure: Default(Boolean, false),
      /**
       * Carry this provider's free-text error `message` onto the thrown typed
       * error, redacted of this call's own credential material. Off by default;
       * mirrors `client.CarryRemoteMessage` in the Go runtime.
       */
      carryRemoteMessage: Default(Boolean, false),
      /**
       * Static, non-secret request defaults, such as
       * `X-Putnami-Observed-Revision`. Operation headers win; credential,
       * identity, tracing and transport headers are refused. Keep secrets in
       * `credentials`. Mirrors `ServiceBinding.Headers` in the Go runtime.
       */
      headers: Optional(MapOf(String, String)),
      credentials: Optional(
        MapOf(String, {
          source: String,
          audience: Optional(String),
          tokenUrl: Optional(String),
          clientId: Optional(String),
          clientSecret: Sensitive(Optional(String)),
          assertionSource: Optional(String),
          assertionAudience: Optional(String),
          assertion: Sensitive(Optional(String)),
          grantType: Optional(String),
          parameters: Sensitive(Optional(MapOf(String, String))),
          tokenRequestFormat: Optional(String),
          value: Sensitive(Optional(String)),
          metadataUrl: Optional(String),
          allowInsecure: Default(Boolean, false),
        }),
      ),
    }),
    {},
  ),
});

/** Interned DI token auto-registered by Application config bootstrap. */
export function generatedClientsConfigToken() {
  return configToken(GeneratedClientsConfig);
}

/**
 * Resolve a service URL from config.
 *
 * Reads from the `client.services.{name}` config path, or falls back to
 * environment variable `CLIENT_SERVICE_{NAME}_URL`.
 */
export function resolveServiceUrl(name: string, configuredUrl?: string): string {
  if (configuredUrl) {
    // Scheme-validate the configured URL too: it flows straight into the
    // transports and fetch (SSRF).
    return assertHttpUrl(configuredUrl, `config (client.services.${name})`);
  }

  // Try environment variable: CLIENT_SERVICE_USERS_API_URL
  const envKey = `CLIENT_SERVICE_${name.toUpperCase().replace(/-/g, '_')}_URL`;
  const envUrl = process.env[envKey];
  if (envUrl) {
    return assertHttpUrl(envUrl, envKey);
  }

  throw new Error(
    `No URL configured for service "${name}". ` + `Set it in config (client.services.${name}) or env var (${envKey}).`,
  );
}
