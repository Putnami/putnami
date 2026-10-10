import type { ConfigSource } from '@putnami/runtime';
import { getEnv } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import { discoverTokenSource, type TokenSource } from './token-source';
import { syncFetch, type SyncFetchRequest } from './sync-fetch';
import { loadConfigSnapshot } from './remote-snapshot';
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

interface RemoteConfigSourceOptions {
  /** Legacy base URL. The source appends /api/configs/resolve and POSTs the request body. */
  serverUrl?: string;
  /** Exact /api/configs/resolve URL. The source fetches this URL as-is with GET. */
  url?: string;
  /**
   * Canonical config-server audience. A legacy raw Cloud Run fetch URL can become
   * unreachable after ingress is locked to the load balancer; on its 404 only,
   * the source retries the same request through this origin.
   */
  audienceFallback?: string;
  appName: string;
  version?: string;
  /**
   * Sends pinned=true alongside version so the server resolves ONLY the frozen
   * version layer and fails loud on a missing pin. Set only for a real
   * CONFIG_VERSION revision pin; a bare version stays additive.
   */
  pinned?: boolean;
  environment?: string;
  schemaHash?: string;
  /**
   * Static bearer token for the Authorization header. Ignored when
   * `tokenSource` is set.
   */
  token?: string;
  /**
   * Resolves the bearer per request — lets GCP runtimes mint a fresh
   * ID token from the metadata server without a process restart.
   * Takes precedence over `token` when set.
   */
  tokenSource?: TokenSource;
  timeout?: number;
  retryBudget?: number;
  required?: boolean;
  /**
   * gs://<bucket>/<object> pointer to the durable, KMS-encrypted last-known-good
   * config snapshot the release path stamped for this workload.
   * When set, a config-server fetch that exhausts its retry budget falls back to
   * this snapshot instead of failing startup, so a control-plane outage no longer
   * blocks a cold start.
   */
  snapshotUri?: string;
  /**
   * Cloud KMS cryptoKey resource name that decrypts the snapshot object. Ignored
   * when `snapshotUri` is empty.
   */
  snapshotKmsKey?: string;
}

type RemoteConfigErrorContext = Record<string, unknown> & {
  url?: string;
  method?: string;
  statusCode?: number;
  authPresent?: boolean;
  authMode?: string;
  appName?: string;
  environment?: string;
};

export class RemoteConfigError extends Error {
  readonly url?: string;
  readonly method?: string;
  readonly statusCode?: number;
  readonly authPresent?: boolean;
  readonly authMode?: string;
  readonly appName?: string;
  readonly environment?: string;

  constructor(message: string, cause?: unknown, context: RemoteConfigErrorContext = {}) {
    const errorCause = cause instanceof Error ? cause : cause ? new Error(String(cause)) : undefined;
    const detail = errorCause?.message;
    super(detail ? `${message}: ${detail}` : message, { cause: errorCause });
    this.name = 'RemoteConfigError';
    this.url = context.url;
    this.method = context.method;
    this.statusCode = context.statusCode;
    this.authPresent = context.authPresent;
    this.authMode = context.authMode;
    this.appName = context.appName;
    this.environment = context.environment;
  }
}

/** Matches the protocol ResolveResponse shape. */
export interface ResolveResponse {
  config: Record<string, unknown>;
  resolved: boolean;
  schemaMatch: boolean;
  layers?: Array<{ dimension: string; priority: number }>;
  warnings?: string[];
}

type RemoteConfigCacheEntry =
  | { status: 'resolved'; config: Record<string, unknown>; response: ResolveResponse }
  | { status: 'not-resolved'; failure: RemoteAttemptFailure }
  | { status: 'failed'; failure: RemoteAttemptFailure };

const remoteConfigCache = new Map<string, RemoteConfigCacheEntry>();

/**
 * Fetches configuration from a Putnami config server.
 *
 * Priority 50: between local file sources (10-35) and CONFIG_DATA (60).
 * On optional-source failure, returns undefined so local sources provide config.
 * On required-source failure (Cloud Run/prod discovery), throws to fail startup.
 */
export class RemoteConfigSource implements ConfigSource {
  readonly name = 'config-server';
  readonly priority = 50;

  private cached: Record<string, unknown> | undefined | null = null;

  constructor(private options: RemoteConfigSourceOptions) {
    this.options.timeout ??= DEFAULT_CONFIG_SERVER_TIMEOUT_MS;
    this.options.environment ??= getEnv();
    if (this.options.required) {
      this.options.retryBudget ??= DEFAULT_CONFIG_SERVER_RETRY_BUDGET_MS;
    }
    if (!this.options.url && !this.options.serverUrl) {
      throw new Error('RemoteConfigSource requires url or serverUrl');
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
      const cached = remoteConfigCache.get(cacheKey);
      if (cached) {
        return this.resolveCached(cached);
      }

      const result = this.fetchOnce(request, cacheKey);
      if (!('failure' in result)) {
        return result.config;
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
      useLogger('config-server').warn('config-server fetch failed; retrying', {
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
  ): { config: Record<string, unknown> } | { failure: RemoteAttemptFailure } {
    try {
      let effectiveRequest = request;
      let fetched = syncFetch(effectiveRequest);
      if (fetched.status === 404) {
        const fallbackRequest = audienceFallbackRequest(effectiveRequest, this.options.audienceFallback);
        if (fallbackRequest) {
          effectiveRequest = fallbackRequest;
          fetched = syncFetch(effectiveRequest);
        }
      }
      if (fetched.status !== 200) {
        const cause = new Error(`status ${fetched.status}`);
        const context = this.failureContext(effectiveRequest, fetched.status);
        const failure =
          fetched.status === 408 || fetched.status === 425 || fetched.status === 429 || fetched.status >= 500
            ? retryableRemoteFailure('config-server returned non-200', cause, context)
            : terminalRemoteFailure('config-server returned non-200', cause, context);
        return { failure };
      }
      const response = JSON.parse(fetched.body) as ResolveResponse;
      if (!response.resolved) {
        if (isEmptySchemaMatch(response)) {
          const config = {};
          remoteConfigCache.set(cacheKey, { status: 'resolved', config, response });
          return { config };
        }
        const failure = terminalRemoteFailure(
          'config-server did not resolve config',
          undefined,
          this.failureContext(request, 200),
        );
        remoteConfigCache.set(cacheKey, { status: 'not-resolved', failure });
        return { failure };
      }

      remoteConfigCache.set(cacheKey, { status: 'resolved', config: response.config, response });
      if (response.schemaMatch === false) {
        const logger = useLogger('config-server');
        logger.warn('config-server schema mismatch', {
          serverUrl: this.options.serverUrl,
          appName: this.options.appName,
          environment: this.options.environment,
        });
      }
      if (response.warnings?.length) {
        const logger = useLogger('config-server');
        for (const warning of response.warnings) {
          logger.warn('config-server warning', {
            serverUrl: this.options.serverUrl,
            appName: this.options.appName,
            environment: this.options.environment,
            warning,
          });
        }
      }

      return { config: response.config };
    } catch (cause) {
      return { failure: retryableRemoteFailure('config-server fetch failed', cause, this.failureContext(request)) };
    }
  }

  private resolveCached(cached: RemoteConfigCacheEntry): Record<string, unknown> | undefined {
    if (cached.status === 'resolved') {
      return cached.config;
    }
    // A cached 'failed'/'not-resolved' entry (poisoned by finalizeFailure from
    // an earlier source instance in this process) must still get the snapshot
    // fallback — otherwise a second instance would skip the durable snapshot and
    // fail startup when the first already recorded the outage.
    const snapshot = this.loadSnapshotFallback(cached.failure);
    if (snapshot) {
      return snapshot;
    }
    return this.fail(cached.failure.message, cached.failure.cause, cached.failure.context as RemoteConfigErrorContext);
  }

  /**
   * Attempts the durable last-known-good config snapshot after
   * the config server was unreachable across the retry budget. Returns the decoded
   * tree on success, undefined when no snapshot is configured, the failure is
   * terminal rather than a retryable outage, or the snapshot itself could not be
   * loaded (the caller then applies the unchanged terminal-failure semantics: a
   * required source throws, an optional one returns undefined).
   *
   * The fallback is gated to retryable (outage-class) failures on purpose. A
   * terminal failure is authoritative and must stay terminal: a 401/403 (config
   * access revoked) or a 404 / resolved:false (config deleted) means the workload
   * is no longer entitled to that config, so booting it on a stale, secret-bearing
   * snapshot would defeat the revocation. A 404 from a legacy raw Cloud Run URL
   * first gets one canonical-audience retry; only its resulting failure reaches
   * this policy.
   *
   * On success it emits a loud, structured WARN. Config loading precedes the
   * metrics pipeline (it runs before the DI graph exists), so this log line — not
   * an OTel counter — is the "running on snapshot" telemetry signal a log-based
   * alert keys on. The stale-config mode is deliberately noisy.
   */
  private loadSnapshotFallback(failure: RemoteAttemptFailure): Record<string, unknown> | undefined {
    const snapshotUri = this.options.snapshotUri;
    if (!snapshotUri) {
      return undefined;
    }
    if (!failure.retryable) {
      return undefined;
    }
    const logger = useLogger('config-server');
    try {
      const tree = loadConfigSnapshot(
        snapshotUri,
        this.options.snapshotKmsKey ?? '',
        this.options.timeout ?? DEFAULT_CONFIG_SERVER_TIMEOUT_MS,
      );
      logger.warn('config server unreachable; booting on durable config snapshot (stale-config mode)', {
        appName: this.options.appName,
        environment: this.options.environment,
        snapshotUri,
        remoteError: remoteFailureDetail(failure),
      });
      return tree;
    } catch (cause) {
      logger.warn('config snapshot fallback failed; no durable config available', {
        appName: this.options.appName,
        environment: this.options.environment,
        snapshotUri,
        remoteError: remoteFailureDetail(failure),
        snapshotError: cause instanceof Error ? cause.message : String(cause),
      });
      return undefined;
    }
  }

  private buildHeaders(): Record<string, string> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    const bearer = this.options.tokenSource?.token() ?? this.options.token;
    if (bearer) {
      headers['Authorization'] = `Bearer ${bearer}`;
    }
    return headers;
  }

  private buildRequest(headers: Record<string, string>, timeoutMs: number) {
    if (this.options.url) {
      // The exact-URL form is a GET with the resolve query baked in, so the config
      // version pin must ride the query string — the POST body (which
      // carries version + pinned) is never sent on this path.
      const url = this.options.version
        ? withVersionQuery(this.options.url, this.options.version, this.options.pinned ?? false)
        : this.options.url;
      return { url, method: 'GET' as const, headers, timeoutMs, throwOnHttpError: false };
    }

    const pinned = Boolean(this.options.pinned && this.options.version);
    const body = JSON.stringify({
      appName: this.options.appName,
      version: this.options.version || undefined,
      pinned: pinned || undefined,
      environment: this.options.environment,
      schemaHash: this.options.schemaHash || undefined,
    });

    return {
      url: `${this.options.serverUrl?.replace(/\/+$/, '')}/api/configs/resolve`,
      method: 'POST' as const,
      headers,
      body,
      timeoutMs,
      throwOnHttpError: false,
    };
  }

  private fail(message: string, cause?: unknown, context: RemoteConfigErrorContext = {}): undefined {
    const detail = cause instanceof Error ? cause.message : cause ? String(cause) : '';
    if (this.options.required) {
      throw new RemoteConfigError(message, cause, {
        appName: this.options.appName,
        environment: this.options.environment,
        ...context,
      });
    }
    useLogger('config-server').warn(message, {
      appName: this.options.appName,
      environment: this.options.environment,
      ...context,
      error: detail || undefined,
    });
    return undefined;
  }

  private finalizeFailure(
    cacheKey: string | undefined,
    failure: RemoteAttemptFailure,
  ): Record<string, unknown> | undefined {
    // Record the remote outage in the module cache so peer instances short-circuit
    // the retry loop. The remote is cached as 'failed' even when the snapshot then
    // serves the boot config: the snapshot is never cached as a 'resolved' remote,
    // so a later reachable config server is still honoured.
    if (cacheKey && !remoteConfigCache.has(cacheKey)) {
      remoteConfigCache.set(cacheKey, { status: 'failed', failure });
    }
    const snapshot = this.loadSnapshotFallback(failure);
    if (snapshot) {
      return snapshot;
    }
    return this.fail(failure.message, failure.cause, failure.context as RemoteConfigErrorContext | undefined);
  }

  private failureContext(request: SyncFetchRequest, statusCode?: number): RemoteConfigErrorContext {
    const authorization = request.headers?.['Authorization'];
    return {
      url: request.url,
      method: request.method,
      statusCode,
      authPresent: Boolean(authorization),
      authMode: authorization ? 'bearer' : 'none',
    };
  }
}

export function resetRemoteConfigSourceCacheForTest(): void {
  remoteConfigCache.clear();
}

/**
 * Creates a RemoteConfigSource from environment variables.
 * Returns undefined if CONFIG_SERVER_URL is not set. Exact
 * /api/configs/resolve URLs are fetched as-is with GET; legacy base URLs use
 * POST /api/configs/resolve.
 *
 * Bearer-token precedence: operator token env vars > GCP workload identity >
 * unauthenticated. The same identity is presented to both
 * /api/configs/resolve and /api/secrets/resolve.
 */
export function discoverRemoteSource(): RemoteConfigSource | undefined {
  const configuredURL = configServerURLFromEnv();
  if (!configuredURL) {
    return undefined;
  }
  const exactResolveURL = configServerURLIsResolveEndpoint(configuredURL);
  return new RemoteConfigSource({
    url: exactResolveURL ? configuredURL : undefined,
    serverUrl: exactResolveURL ? undefined : configuredURL,
    appName: process.env['APP_NAME'] || '',
    // The config version pin is CONFIG_VERSION alone (the pin the deploy
    // stamps), sent with pinned=true for EXCLUSIVE, fail-loud resolution. It
    // deliberately does NOT fall back to APP_VERSION: APP_VERSION carries the
    // build semver at runtime, and sending it as a pin would fail every boot on a
    // never-frozen version. Secrets are never pinned (remote-secrets-source).
    version: process.env['CONFIG_VERSION'],
    pinned: Boolean(process.env['CONFIG_VERSION']),
    environment: getEnv(),
    schemaHash: process.env['SCHEMA_HASH'],
    tokenSource: discoverTokenSource(configuredURL),
    timeout: configServerTimeoutFromEnv(),
    retryBudget: configServerRetryBudgetFromEnv(),
    required: remoteConfigRequired(),
    audienceFallback: process.env['CONFIG_SERVER_AUDIENCE']?.trim() || undefined,
    snapshotUri: process.env['CONFIG_SNAPSHOT_URI']?.trim() || undefined,
    snapshotKmsKey: process.env['CONFIG_SNAPSHOT_KMS_KEY']?.trim() || undefined,
  });
}

// Bun and Node do not agree across versions on whether assigning `undefined`
// to process.env deletes a key or stores the literal string "undefined". Treat
// the common orchestration sentinels as absent at the runtime boundary so an
// unset optional source can never turn into a request to an invalid origin.
export function configServerURLFromEnv(): string | undefined {
  const value = process.env['CONFIG_SERVER_URL']?.trim();
  if (!value) {
    return undefined;
  }
  const normalized = value.toLowerCase();
  if (normalized === 'undefined' || normalized === 'null') {
    return undefined;
  }
  return value;
}

function audienceFallbackRequest(
  request: SyncFetchRequest,
  audience: string | undefined,
): SyncFetchRequest | undefined {
  if (!audience) {
    return undefined;
  }
  try {
    const current = new URL(request.url);
    const fallback = new URL(audience);
    if (current.origin === fallback.origin) {
      return undefined;
    }
    current.protocol = fallback.protocol;
    current.host = fallback.host;
    return { ...request, url: current.toString() };
  } catch {
    // Treat an invalid configured audience as unavailable. The primary request
    // still produces its normal terminal 404 diagnostic.
    return undefined;
  }
}

/**
 * Adds `version=<version>` (and `pinned=true` when pinned) to an exact resolve
 * URL's query string so the config server pins the resolve to a frozen revision.
 * An existing `version` query is left untouched (an operator-set pin
 * wins); an unparseable URL is returned as-is.
 */
function withVersionQuery(rawUrl: string, version: string, pinned: boolean): string {
  try {
    const parsed = new URL(rawUrl);
    if (parsed.searchParams.get('version')) {
      return rawUrl;
    }
    parsed.searchParams.set('version', version);
    if (pinned) {
      parsed.searchParams.set('pinned', 'true');
    }
    return parsed.toString();
  } catch {
    return rawUrl;
  }
}

export function configServerURLIsResolveEndpoint(value: string): boolean {
  try {
    const parsed = new URL(value);
    return parsed.pathname.replace(/\/+$/, '').endsWith('/api/configs/resolve');
  } catch {
    return false;
  }
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

function isEmptySchemaMatch(response: ResolveResponse): boolean {
  return (
    response.schemaMatch === true &&
    isEmptyRecord(response.config) &&
    (!response.warnings || response.warnings.length === 0)
  );
}

function isEmptyRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 0);
}

function remoteConfigRequired(): boolean {
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
