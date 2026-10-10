import { useLogger } from '@putnami/runtime';
import { getEnv } from '@putnami/runtime';
import type { ConfigSource } from '@putnami/runtime';
import { configServerURLFromEnv, configServerURLIsResolveEndpoint } from './remote-config-source';
import {
  DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS,
  DEFAULT_CONFIG_SERVER_TIMEOUT_MS,
  configServerRetryBudgetFromEnv,
  configServerTimeoutFromEnv,
  remoteFailureDetail,
  remoteRetryDelayMs,
  retryableRemoteFailure,
  sleepSync,
  terminalRemoteFailure,
  type RemoteAttemptFailure,
} from './remote-retry';
import { syncFetch, type SyncFetchRequest } from './sync-fetch';
import { discoverTokenSource, type TokenSource } from './token-source';

interface RemoteSecretsSourceOptions {
  serverUrl: string;
  appName: string;
  version?: string;
  environment?: string;
  schemaHash?: string;
  /**
   * Static bearer token for the Authorization header. Ignored when
   * `tokenSource` is set.
   */
  token?: string;
  /**
   * Resolves the bearer per request — mirrors RemoteConfigSource so
   * config + secrets share one identity. Takes precedence over `token`.
   */
  tokenSource?: TokenSource;
  timeout?: number;
  retryBudget?: number;
  required?: boolean;
}

/** Matches the protocol ResolveSecretsResponse shape. */
interface ResolveSecretsResponse {
  secrets: Record<string, unknown>;
  resolved: boolean;
  schemaMatch: boolean;
  layers?: Array<{ dimension: string; priority: number }>;
  warnings?: string[];
}

type RemoteSecretsCacheEntry =
  | { status: 'resolved'; secrets: Record<string, unknown>; response: ResolveSecretsResponse }
  | { status: 'not-resolved' }
  | { status: 'failed'; cause: unknown };

const remoteSecretsCache = new Map<string, RemoteSecretsCacheEntry>();

/**
 * Fetches secrets from a Putnami config server.
 *
 * Priority 55: just above RemoteConfigSource so secret values can override
 * non-sensitive defaults with the same key, but field-level `Env(...)`
 * resolution (priority 80) still wins.
 *
 * On optional-source failure, returns undefined so local sources provide
 * secrets. On required-source failure (Cloud Run/prod discovery), throws to
 * fail startup.
 */
export class RemoteSecretsSource implements ConfigSource {
  readonly name = 'secrets-server';
  readonly priority = 55;

  private cached: Record<string, unknown> | undefined | null = null;

  constructor(private options: RemoteSecretsSourceOptions) {
    this.options.timeout ??= DEFAULT_CONFIG_SERVER_TIMEOUT_MS;
    this.options.environment ??= getEnv();
    if (this.options.required) {
      this.options.retryBudget ??= DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS;
    }
  }

  load(): Record<string, unknown> | undefined {
    if (this.cached !== null) {
      return this.cached;
    }
    this.cached = this.fetch();
    return this.cached;
  }

  private fetch(): Record<string, unknown> | undefined {
    const retryBudget = this.options.required ? (this.options.retryBudget ?? DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS) : 0;
    const deadline = retryBudget > 0 ? Date.now() + retryBudget : 0;
    let lastFailure: RemoteAttemptFailure | undefined;
    let lastCacheKey: string | undefined;

    for (let failedAttempts = 0; ; ) {
      let timeoutMs = this.options.timeout ?? DEFAULT_CONFIG_SERVER_TIMEOUT_MS;
      if (deadline > 0) {
        const remaining = deadline - Date.now();
        if (remaining <= 0 && lastFailure) {
          return this.finalizeFailure(lastCacheKey, lastFailure);
        }
        if (remaining > 0 && remaining < timeoutMs) {
          timeoutMs = remaining;
        }
      }

      const request = this.buildRequest(this.buildHeaders(), timeoutMs);
      const cacheKey = remoteRequestCacheKey(request);
      const cached = remoteSecretsCache.get(cacheKey);
      if (cached) {
        return this.resolveCached(cached);
      }

      const result = this.fetchOnce(request, cacheKey);
      if (!('failure' in result)) {
        return result.secrets;
      }

      lastFailure = result.failure;
      lastCacheKey = cacheKey;
      if (!this.options.required || !result.failure.retryable || retryBudget <= 0) {
        return this.finalizeFailure(cacheKey, result.failure);
      }

      failedAttempts += 1;
      const delayMs = remoteRetryDelayMs(failedAttempts);
      if (deadline > 0 && deadline - Date.now() <= delayMs) {
        return this.finalizeFailure(cacheKey, result.failure);
      }
      useLogger('secrets-server').warn('secrets-server fetch failed; retrying', {
        appName: this.options.appName,
        environment: this.options.environment,
        attempt: failedAttempts,
        delayMs,
        error: remoteFailureDetail(result.failure),
      });
      sleepSync(delayMs);
    }
  }

  private fetchOnce(
    request: SyncFetchRequest,
    cacheKey: string,
  ): { secrets: Record<string, unknown> } | { failure: RemoteAttemptFailure } {
    try {
      const fetched = syncFetch(request);
      if (fetched.status !== 200) {
        const failure =
          fetched.status === 408 || fetched.status === 425 || fetched.status === 429 || fetched.status >= 500
            ? retryableRemoteFailure('secrets-server returned non-200', `status ${fetched.status}`)
            : terminalRemoteFailure('secrets-server returned non-200', `status ${fetched.status}`);
        return { failure };
      }
      const response = JSON.parse(fetched.body) as ResolveSecretsResponse;
      if (!response.resolved) {
        remoteSecretsCache.set(cacheKey, { status: 'not-resolved' });
        return { failure: terminalRemoteFailure('secrets-server did not resolve secrets') };
      }

      remoteSecretsCache.set(cacheKey, { status: 'resolved', secrets: response.secrets, response });
      if (response.schemaMatch === false) {
        const logger = useLogger('secrets-server');
        logger.warn('secrets-server schema mismatch', {
          appName: this.options.appName,
          environment: this.options.environment,
        });
      }
      if (response.warnings?.length) {
        const logger = useLogger('secrets-server');
        for (const warning of response.warnings) {
          logger.warn('secrets-server warning', {
            appName: this.options.appName,
            environment: this.options.environment,
            warning,
          });
        }
      }

      return { secrets: response.secrets };
    } catch (cause) {
      return { failure: retryableRemoteFailure('secrets-server fetch failed', cause) };
    }
  }

  private buildRequest(headers: Record<string, string>, timeoutMs: number): SyncFetchRequest {
    const body = JSON.stringify({
      appName: this.options.appName,
      version: this.options.version || undefined,
      environment: this.options.environment,
      schemaHash: this.options.schemaHash || undefined,
    });

    return {
      url: `${this.options.serverUrl.replace(/\/+$/, '')}/api/secrets/resolve`,
      method: 'POST',
      headers,
      body,
      timeoutMs,
      throwOnHttpError: false,
    };
  }

  private buildHeaders(): Record<string, string> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    const bearer = this.options.tokenSource?.token() ?? this.options.token;
    if (bearer) {
      headers['Authorization'] = `Bearer ${bearer}`;
    }
    return headers;
  }

  private resolveCached(cached: RemoteSecretsCacheEntry): Record<string, unknown> | undefined {
    if (cached.status === 'resolved') {
      return cached.secrets;
    }
    if (cached.status === 'not-resolved') {
      return this.fail('secrets-server did not resolve secrets');
    }
    return this.fail('secrets-server fetch failed', cached.cause);
  }

  private fail(message: string, cause?: unknown): undefined {
    const detail = cause instanceof Error ? cause.message : cause ? String(cause) : '';
    if (this.options.required) {
      throw new Error(detail ? `${message}: ${detail}` : message);
    }
    useLogger('secrets-server').warn(message, {
      appName: this.options.appName,
      environment: this.options.environment,
      error: detail || undefined,
    });
    return undefined;
  }

  private finalizeFailure(cacheKey: string | undefined, failure: RemoteAttemptFailure): undefined {
    if (cacheKey && !remoteSecretsCache.has(cacheKey)) {
      remoteSecretsCache.set(cacheKey, { status: 'failed', cause: failure.cause ?? failure.message });
    }
    return this.fail(failure.message, failure.cause);
  }
}

export function resetRemoteSecretsSourceCacheForTest(): void {
  remoteSecretsCache.clear();
}

/**
 * Creates a RemoteSecretsSource from environment variables.
 * Returns undefined if CONFIG_SERVER_URL is not set or already points to an
 * exact /api/configs/resolve URL that carries merged config/secrets.
 *
 * Bearer-token precedence matches discoverRemoteSource: operator token env
 * vars win, then GCP workload identity (via K_SERVICE / GOOGLE_CLOUD_PROJECT),
 * otherwise unauthenticated. The same identity is presented to both resolve
 * endpoints.
 */
export function discoverRemoteSecretsSource(): RemoteSecretsSource | undefined {
  const serverUrl = configServerURLFromEnv();
  if (!serverUrl) {
    return undefined;
  }
  if (configServerURLIsResolveEndpoint(serverUrl)) {
    return undefined;
  }
  return new RemoteSecretsSource({
    serverUrl,
    appName: process.env['APP_NAME'] || '',
    version: process.env['APP_VERSION'],
    environment: getEnv(),
    schemaHash: process.env['SCHEMA_HASH'],
    tokenSource: discoverTokenSource(serverUrl),
    timeout: configServerTimeoutFromEnv(),
    retryBudget: configServerRetryBudgetFromEnv(),
    required: remoteSecretsRequired(),
  });
}

function remoteRequestCacheKey(request: SyncFetchRequest): string {
  const headers = Object.entries(request.headers || {}).sort(([a], [b]) => a.localeCompare(b));
  return JSON.stringify({
    url: request.url,
    method: request.method,
    body: request.body,
    headers,
  });
}

function remoteSecretsRequired(): boolean {
  const explicit = process.env['CONFIG_SERVER_REQUIRED']?.trim().toLowerCase();
  if (explicit) {
    return ['1', 'true', 'yes', 'on'].includes(explicit);
  }
  const appEnv = process.env['APP_ENV']?.trim().toLowerCase();
  const nodeEnv = runtimeEnv('NODE_ENV')?.trim().toLowerCase();
  return (
    Boolean(process.env.K_SERVICE) ||
    nodeEnv === 'production' ||
    nodeEnv === 'prod' ||
    appEnv === 'prod' ||
    appEnv === 'production'
  );
}

function runtimeEnv(name: string): string | undefined {
  return process.env[name];
}
