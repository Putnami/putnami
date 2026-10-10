import {
  eventServerTransport,
  type EventServerRetryConfig,
  type EventServerTokenRequest,
  type EventServerTokenSource,
  type Transport,
} from '@putnami/events';
import { request as directHttpRequest, type IncomingHttpHeaders } from 'node:http';
import { Readable } from 'node:stream';

const EVENT_SERVER_TRANSPORT = 'eventserver';
const EVENT_SERVER_PROTOCOL = 'putnami.events.v1';
const EVENT_SERVER_CONTRACT_VERSION = 1;
const DEFAULT_REFRESH_SKEW_MS = 60 * 1000;
const DEFAULT_CREDENTIAL_TIMEOUT_MS = 5000;
const MAX_CREDENTIAL_RESPONSE_BYTES = 64 * 1024;
const MANAGED_EVENT_SERVER_KEYS = new Set([
  'contractVersion',
  'endpoint',
  'audience',
  'protocol',
  'workspaceId',
  'environment',
  'workload',
  'topologyGenerationId',
]);

export const GOOGLE_METADATA_IDENTITY_ENDPOINT =
  'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity';

/** The complete control-plane-owned Event Server binding for a managed workload. */
export interface ManagedEventServerBinding {
  contractVersion: number;
  endpoint: string;
  audience: string;
  protocol: string;
  workspaceId: string;
  environment: string;
  workload: string;
  topologyGenerationId: string;
}

/** Resolved events config accepted by the cloud Event Server destination. */
export interface ManagedEventServerEventsConfig {
  transport: string;
  eventServer: ManagedEventServerBinding;
  /** A legacy direct-Pub/Sub binding must be absent in Event Server mode. */
  pubsub?: Record<string, unknown>;
}

/** Persisted route identity used by durable publishers and outbox relays. */
export interface EventServerRouteDescriptor {
  transport: string;
  topologyGenerationId: string;
}

export interface EventServerCredentialCacheOptions {
  /** Do not serve a token this close to expiry. Default: one minute. */
  refreshSkewMs?: number;
  /** Independent timeout for a shared credential refresh. Default: five seconds. */
  acquisitionTimeoutMs?: number;
  /** Deterministic clock seam. */
  now?: () => number;
}

export interface GoogleEventServerTokenSourceOptions extends EventServerCredentialCacheOptions {
  /** Deterministic HTTP seam for tests. */
  fetch?: typeof globalThis.fetch;
}

export interface EventServerDestinationOptions {
  /**
   * Explicit provider credential seam for tests and local/self-managed execution.
   * When absent, the fixed Google metadata identity endpoint is used and fails
   * closed outside a Google managed runtime.
   */
  tokenSource?: EventServerTokenSource;
  /** A persisted route pin. Omit for an ordinary publish using the active config. */
  route?: EventServerRouteDescriptor;
  credentialCache?: EventServerCredentialCacheOptions;
  metadata?: Pick<GoogleEventServerTokenSourceOptions, 'fetch'>;
  /** Framework HTTP/retry seams. They cannot override binding identity. */
  transport?: {
    fetch?: typeof globalThis.fetch;
    random?: () => number;
    sleep?: (delayMs: number, signal: AbortSignal) => Promise<void>;
    requestTimeoutMs?: number;
    maxRequestBytes?: number;
    retry?: EventServerRetryConfig;
  };
}

interface CachedCredential {
  token: string;
  expiresAtMs: number;
}

/**
 * Build the TypeScript cloud destination for an exact managed Event Server
 * generation. This function never constructs or selects a Pub/Sub transport.
 */
export function eventServerDestination(
  config: ManagedEventServerEventsConfig,
  options: EventServerDestinationOptions = {},
): Transport {
  const binding = validateManagedEventServerConfig(config);
  const route = validateRouteDescriptor(
    options.route ?? {
      transport: EVENT_SERVER_TRANSPORT,
      topologyGenerationId: binding.topologyGenerationId,
    },
  );
  assertRouteMatchesBinding(route, binding);

  const credentialSource = options.tokenSource
    ? cachedEventServerTokenSource(options.tokenSource, options.credentialCache)
    : googleEventServerTokenSource({
        ...options.credentialCache,
        ...options.metadata,
      });

  const transportOptions = options.transport ?? {};
  const transport = eventServerTransport({
    contractVersion: EVENT_SERVER_CONTRACT_VERSION,
    endpoint: binding.endpoint,
    audience: binding.audience,
    protocol: EVENT_SERVER_PROTOCOL,
    tokenSource: credentialSource,
    workspaceId: binding.workspaceId,
    environment: binding.environment,
    workload: binding.workload,
    topologyGenerationId: binding.topologyGenerationId,
    fetch: transportOptions.fetch,
    random: transportOptions.random,
    sleep: transportOptions.sleep,
    requestTimeoutMs: transportOptions.requestTimeoutMs,
    maxRequestBytes: transportOptions.maxRequestBytes,
    retry: transportOptions.retry,
  });

  // The wrapper keeps the persisted route descriptor attached to every new
  // publish call. A stale retry fails before the framework asks for a token or
  // sends an Event Server request. Framework-internal retries retain the same
  // immutable transport and request bytes.
  return {
    publish(topic, envelope, publishOptions) {
      assertRouteMatchesBinding(route, binding);
      return transport.publish(topic, envelope, publishOptions);
    },
    subscribe(definition, callback) {
      return transport.subscribe(definition, callback);
    },
    start() {
      return transport.start();
    },
    stop() {
      return transport.stop();
    },
  };
}

/**
 * Google metadata ID-token source with exact JWT audience/expiry validation,
 * an expiry-safe per-audience cache, and one shared refresh per audience.
 */
export function googleEventServerTokenSource(
  options: GoogleEventServerTokenSourceOptions = {},
): EventServerTokenSource {
  // Bun's global fetch honors HTTP_PROXY/HTTPS_PROXY. Production metadata
  // identity must never transit an operator proxy, so only tests may inject
  // fetch; the default client opens a direct one-shot node:http connection.
  const fetchImpl = options.fetch ?? directMetadataIdentityFetch;

  return cachedEventServerTokenSource(async ({ audience, signal }) => {
    const url = new URL(GOOGLE_METADATA_IDENTITY_ENDPOINT);
    url.searchParams.set('audience', audience);
    url.searchParams.set('format', 'full');

    let response: Response;
    try {
      response = await fetchImpl(url, {
        method: 'GET',
        headers: { 'Metadata-Flavor': 'Google' },
        redirect: 'error',
        signal,
      });
    } catch {
      throw credentialError();
    }
    if (!response.ok || response.headers.get('Metadata-Flavor') !== 'Google') {
      throw credentialError();
    }

    const token = await readBoundedCredentialResponse(response);
    if (!token) {
      throw credentialError();
    }
    return token;
  }, options);
}

function directMetadataIdentityFetch(input: string | URL | Request, init?: RequestInit): Promise<Response> {
  const url = new URL(input instanceof Request ? input.url : input.toString());
  return new Promise((resolve, reject) => {
    const request = directHttpRequest(
      url,
      {
        method: init?.method ?? 'GET',
        headers: Object.fromEntries(new Headers(init?.headers).entries()),
        agent: false,
        signal: init?.signal ?? undefined,
      },
      (response) => {
        resolve(
          new Response(Readable.toWeb(response) as unknown as ReadableStream<Uint8Array>, {
            status: response.statusCode ?? 500,
            headers: webHeaders(response.headers),
          }),
        );
      },
    );
    request.once('error', reject);
    request.end();
  });
}

function webHeaders(source: IncomingHttpHeaders): Headers {
  const headers = new Headers();
  for (const [name, value] of Object.entries(source)) {
    if (Array.isArray(value)) {
      for (const item of value) headers.append(name, item);
    } else if (value !== undefined) {
      headers.set(name, value);
    }
  }
  return headers;
}

async function readBoundedCredentialResponse(response: Response): Promise<string> {
  const reader = response.body?.getReader();
  if (!reader) throw credentialError();

  const chunks: Uint8Array[] = [];
  let size: number;
  try {
    size = await readCredentialChunks(reader, chunks, 0);
  } catch {
    throw credentialError();
  }

  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder('utf-8', { fatal: true }).decode(body);
}

async function readCredentialChunks(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  chunks: Uint8Array[],
  size: number,
): Promise<number> {
  const { done, value } = await reader.read();
  if (done) return size;
  const nextSize = size + value.byteLength;
  if (nextSize > MAX_CREDENTIAL_RESPONSE_BYTES) {
    await reader.cancel().catch(() => undefined);
    throw credentialError();
  }
  chunks.push(value);
  return readCredentialChunks(reader, chunks, nextSize);
}

/**
 * Add audience-keyed, expiry-safe caching and coalescing to any provider-owned
 * Event Server credential source. Individual waiter cancellation never aborts
 * the shared refresh used by other publishers.
 */
export function cachedEventServerTokenSource(
  source: EventServerTokenSource,
  options: EventServerCredentialCacheOptions = {},
): EventServerTokenSource {
  if (typeof source !== 'function') {
    throw configError('Event Server token source is required');
  }
  const refreshSkewMs = boundedInteger(
    options.refreshSkewMs,
    DEFAULT_REFRESH_SKEW_MS,
    0,
    15 * 60 * 1000,
    'refreshSkewMs',
  );
  const acquisitionTimeoutMs = boundedInteger(
    options.acquisitionTimeoutMs,
    DEFAULT_CREDENTIAL_TIMEOUT_MS,
    1,
    60 * 1000,
    'acquisitionTimeoutMs',
  );
  const now = options.now ?? Date.now;
  const cached = new Map<string, CachedCredential>();
  const refreshing = new Map<string, Promise<CachedCredential>>();

  return async (request: EventServerTokenRequest): Promise<string> => {
    const audience = exactNonBlank(request.audience, 'token audience');
    if (request.signal.aborted) {
      throw credentialCancelledError();
    }

    const current = cached.get(audience);
    if (current && current.expiresAtMs - now() > refreshSkewMs) {
      return current.token;
    }

    let refresh = refreshing.get(audience);
    if (!refresh) {
      const controller = new AbortController();
      let timeout: ReturnType<typeof setTimeout> | undefined;
      const sourceCompletion = Promise.resolve().then(() => source({ audience, signal: controller.signal }));
      const deadline = new Promise<never>((_resolve, reject) => {
        timeout = setTimeout(() => {
          controller.abort();
          reject(credentialError());
        }, acquisitionTimeoutMs);
      });
      refresh = Promise.race([sourceCompletion, deadline])
        .then((token): CachedCredential => {
          const expiresAtMs = validateJwt(token, audience, now(), refreshSkewMs);
          const credential = { token, expiresAtMs };
          cached.set(audience, credential);
          return credential;
        })
        .catch(() => {
          throw credentialError();
        });
      refreshing.set(audience, refresh);
      // Keep a timed-out non-cooperative source in the map until it actually
      // settles. Later callers fail on the same bounded promise instead of
      // starting an unbounded number of stuck refreshes for one audience.
      const cleanup = () => {
        if (timeout !== undefined) clearTimeout(timeout);
        if (refreshing.get(audience) === refresh) refreshing.delete(audience);
      };
      Promise.allSettled([sourceCompletion, refresh])
        .then(cleanup)
        .catch(() => undefined);
    }

    const credential = await waitForSharedRefresh(refresh, request.signal);
    return credential.token;
  };
}

function validateManagedEventServerConfig(config: ManagedEventServerEventsConfig): Readonly<ManagedEventServerBinding> {
  if (!isRecord(config)) {
    throw configError('events config is required');
  }
  if (config.transport !== EVENT_SERVER_TRANSPORT) {
    throw configError("events.transport must be 'eventserver'");
  }
  if ('pubsub' in config && hasPopulatedLegacyBinding(config.pubsub)) {
    throw configError('events.pubsub must be absent in Event Server mode');
  }
  if (!isRecord(config.eventServer)) {
    throw configError('events.eventServer is required');
  }

  const eventServer = config.eventServer;
  for (const key of Object.keys(eventServer)) {
    if (!MANAGED_EVENT_SERVER_KEYS.has(key)) {
      throw configError('events.eventServer contains an unsupported field');
    }
  }
  if (eventServer.contractVersion !== EVENT_SERVER_CONTRACT_VERSION) {
    throw configError('events.eventServer.contractVersion must be 1');
  }
  if (eventServer.protocol !== EVENT_SERVER_PROTOCOL) {
    throw configError("events.eventServer.protocol must be 'putnami.events.v1'");
  }

  return Object.freeze({
    contractVersion: EVENT_SERVER_CONTRACT_VERSION,
    endpoint: exactHttpsOrigin(eventServer.endpoint, 'endpoint'),
    audience: exactHttpsOrigin(eventServer.audience, 'audience'),
    protocol: EVENT_SERVER_PROTOCOL,
    workspaceId: exactNonBlank(eventServer.workspaceId, 'workspaceId'),
    environment: exactNonBlank(eventServer.environment, 'environment'),
    workload: exactNonBlank(eventServer.workload, 'workload'),
    topologyGenerationId: exactNonBlank(eventServer.topologyGenerationId, 'topologyGenerationId'),
  });
}

function validateRouteDescriptor(route: EventServerRouteDescriptor): Readonly<EventServerRouteDescriptor> {
  if (!isRecord(route) || route.transport !== EVENT_SERVER_TRANSPORT) {
    throw configError("Event Server route transport must be 'eventserver'");
  }
  return Object.freeze({
    transport: EVENT_SERVER_TRANSPORT,
    topologyGenerationId: exactNonBlank(route.topologyGenerationId, 'route topologyGenerationId'),
  });
}

function assertRouteMatchesBinding(
  route: Readonly<EventServerRouteDescriptor>,
  binding: Readonly<ManagedEventServerBinding>,
): void {
  if (route.transport !== EVENT_SERVER_TRANSPORT || route.topologyGenerationId !== binding.topologyGenerationId) {
    throw configError('Event Server route topology generation is stale');
  }
}

function validateJwt(token: unknown, audience: string, nowMs: number, refreshSkewMs: number): number {
  if (
    typeof token !== 'string' ||
    token !== token.trim() ||
    /[\r\n]/.test(token) ||
    new TextEncoder().encode(token).byteLength > MAX_CREDENTIAL_RESPONSE_BYTES ||
    !Number.isFinite(nowMs) ||
    nowMs < 0
  ) {
    throw credentialError();
  }
  const parts = token.split('.');
  if (parts.length !== 3 || parts.some((part) => !part || !/^[A-Za-z0-9_-]+$/.test(part))) {
    throw credentialError();
  }

  let claims: unknown;
  try {
    claims = JSON.parse(Buffer.from(parts[1] as string, 'base64url').toString('utf8')) as unknown;
  } catch {
    throw credentialError();
  }
  if (!isRecord(claims)) {
    throw credentialError();
  }
  const tokenAudience = claims['aud'];
  const expirySeconds = claims['exp'];
  if (tokenAudience !== audience || !Number.isInteger(expirySeconds)) {
    throw credentialError();
  }
  const expiresAtMs = (expirySeconds as number) * 1000;
  if (!Number.isSafeInteger(expiresAtMs) || expiresAtMs - nowMs <= refreshSkewMs) {
    throw credentialError();
  }
  return expiresAtMs;
}

function exactHttpsOrigin(value: unknown, name: string): string {
  const exact = exactNonBlank(value, name);
  let url: URL;
  try {
    url = new URL(exact);
  } catch {
    throw configError(`events.eventServer.${name} must be an HTTPS origin`);
  }
  if (
    url.protocol !== 'https:' ||
    url.username ||
    url.password ||
    url.pathname !== '/' ||
    url.search ||
    url.hash ||
    (exact !== url.origin && exact !== `${url.origin}/`)
  ) {
    throw configError(`events.eventServer.${name} must be an HTTPS origin`);
  }
  return exact;
}

function exactNonBlank(value: unknown, name: string): string {
  if (typeof value !== 'string' || !value || value !== value.trim() || hasControlCharacter(value)) {
    throw configError(`${name} must be a non-empty exact string`);
  }
  return value;
}

function hasPopulatedLegacyBinding(value: unknown): boolean {
  if (value === undefined || value === null) return false;
  return !isRecord(value) || Object.keys(value).length > 0;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function hasControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code <= 31 || code === 127) return true;
  }
  return false;
}

function boundedInteger(value: number | undefined, fallback: number, min: number, max: number, name: string): number {
  const resolved = value ?? fallback;
  if (!Number.isInteger(resolved) || resolved < min || resolved > max) {
    throw configError(`${name} must be an integer between ${min} and ${max}`);
  }
  return resolved;
}

function waitForSharedRefresh<T>(refresh: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(credentialCancelledError());
  return new Promise((resolve, reject) => {
    const onAbort = () => reject(credentialCancelledError());
    signal.addEventListener('abort', onAbort, { once: true });
    refresh.then(
      (value) => {
        signal.removeEventListener('abort', onAbort);
        resolve(value);
      },
      () => {
        signal.removeEventListener('abort', onAbort);
        reject(credentialError());
      },
    );
  });
}

function configError(message: string): Error {
  return new Error(`Managed Event Server destination is invalid: ${message}`);
}

function credentialError(): Error {
  return new Error('Managed Event Server credential acquisition failed');
}

function credentialCancelledError(): Error {
  return new Error('Managed Event Server credential request was cancelled');
}
