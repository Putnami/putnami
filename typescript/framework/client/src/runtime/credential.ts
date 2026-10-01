import type { ClientCredentialProfile, ClientSecurityRequirement } from '@putnami/application';
import { tryContext } from '@putnami/runtime';
import { CLIENT_ID_HEADER } from '../interceptors/auth.interceptor';
import { ClientCredentialError, ClientError } from './errors';
import type { ClientRequest, ClientResponse, Interceptor } from './transport.type';
import { parseServiceUrl } from './url';

const MAX_CREDENTIAL_BODY_BYTES = 64 * 1024;
const DEFAULT_CREDENTIAL_TIMEOUT_MS = 30_000;
const ASSERTION_TYPE = 'urn:ietf:params:oauth:client-assertion-type:jwt-bearer';
const GCP_METADATA_URL =
  'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity';

export type CredentialSourceKind =
  | 'oauth-client-credentials'
  | 'oauth-client-assertion'
  | 'oauth-extension-grant'
  | 'gcp-id-token'
  | 'forwarded-user'
  /** @deprecated Use `forwarded-user`, matching the Go runtime config. */
  | 'forwarded-user-token'
  | 'static';

export interface CredentialRequest {
  serviceId: string;
  clientId: string;
  profile: string;
  audience: string;
  scopes: readonly string[];
  signal?: AbortSignal;
}

export interface Credential {
  value: string;
  expiresAt: Date;
}

export type CredentialProvider = (request: CredentialRequest) => Promise<Credential>;

/** Runtime-only credential material for one provider-declared profile. */
export interface CredentialBinding {
  source: CredentialSourceKind;
  /**
   * Audience the service token is requested for. A deployment value, so it
   * wins over the provider's profile and contract audience for every source
   * that takes one. For `gcp-id-token` an absent audience means the binding
   * URL: Cloud Run verifies a Google ID token against the URL it is presented
   * to, and that URL differs per environment.
   */
  audience?: string;
  tokenUrl?: string;
  clientId?: string;
  clientSecret?: string;
  assertionSource?: 'gcp-id-token';
  assertionAudience?: string;
  assertion?: string;
  /** OAuth extension grant URI, for example a provider-defined API-key exchange. */
  grantType?: string;
  /** Provider-defined grant parameters. Values are deployment secrets. */
  parameters?: Readonly<Record<string, string>>;
  tokenRequestFormat?: 'form' | 'json';
  value?: string;
  metadataUrl?: string;
  allowInsecure?: boolean;
  /** Low-level seam for tests and external identity systems. */
  provider?: CredentialProvider;
  /**
   * Re-mints a `forwarded-user` token once after a declared or undeclared
   * 401, mirroring a hand-written HTTP client's own single-remint contract
   * (for example a CLI re-authenticating a stale or expiring workspace
   * session and replaying the request). Consulted only for the
   * forwarded-user source; every other source already owns its own
   * acquisition and retry. Absent disables the remint retry, leaving every
   * existing binding's behavior unchanged. Mirrors the Go runtime's
   * `CredentialBinding.Refresh` (service_binding.go).
   */
  refresh?: (signal?: AbortSignal) => Promise<string>;
}

interface AuthContext {
  __authorizationHeader?: string;
}

interface CachedCredential {
  credential: Credential;
  fetchedAt: number;
}

interface ActiveAcquisition {
  promise: Promise<Credential>;
}

/**
 * The application registry is closed.
 *
 * Distinct from an ordinary acquisition failure so a caller can tell "this
 * application is shutting down" from "the identity provider is broken" — the
 * first is expected and final, the second is worth retrying. The Go runtime
 * draws the same line with its own `client.closed` code; here the distinction
 * is the type, because the stable code vocabulary lives in `errors.ts`.
 */
export class CredentialRegistryClosedError extends ClientCredentialError {
  constructor() {
    super('credential manager is closed');
    this.name = 'CredentialRegistryClosedError';
  }
}

/** Seams a test needs to drive expiry without waiting for it. */
export interface CredentialManagerOptions {
  /**
   * Reads the current instant. Injected so freshness, expiry and renewal can be
   * driven by moving a clock rather than by sleeping — a real sleep proves the
   * test waited, not that the runtime renewed.
   */
  readonly now?: () => number;
}

/** Exact cache entry selected for one credential requirement. */
interface AcquiredCredential {
  readonly credential: Credential;
  readonly cacheKey: string;
}

interface RejectedCredential {
  readonly cacheKey: string;
  readonly value: string;
}

const requestCredentials = new WeakMap<
  ClientRequest,
  { readonly manager: CredentialManager; readonly acquired: readonly RejectedCredential[] }
>();

/**
 * Per-binding token cache. Values are keyed by the complete non-secret
 * identity and refreshes for one identity collapse to one acquisition.
 */
export class CredentialManager {
  private readonly cache = new Map<string, CachedCredential>();
  private readonly active = new Map<string, ActiveAcquisition>();
  private readonly providerIdentities = new WeakMap<CredentialProvider, number>();
  private readonly disposeController = new AbortController();
  private readonly now: () => number;
  private nextProviderIdentity = 1;
  private disposed = false;

  constructor(options: CredentialManagerOptions = {}) {
    this.now = options.now ?? Date.now;
  }

  async acquire(
    request: CredentialRequest,
    binding: CredentialBinding,
    maxDurationMs: number,
  ): Promise<AcquiredCredential> {
    this.assertOpen();
    const normalized = { ...request, scopes: sortedUnique(request.scopes), signal: undefined };
    const key = JSON.stringify({ request: normalized, source: await this.sourceIdentity(binding) });
    this.assertOpen();
    const cached = this.cache.get(key);
    if (cached && isFresh(cached, this.now())) return { credential: cached.credential, cacheKey: key };

    let active = this.active.get(key);
    if (!active) {
      const promise = this.acquireLeader({ ...request, scopes: normalized.scopes }, binding, maxDurationMs).finally(
        () => {
          this.active.delete(key);
        },
      );
      active = { promise };
      this.active.set(key, active);
      promise.then(
        (credential) => {
          // An acquisition that lands while `dispose` is running must not write
          // to a cache the caller has already been told is empty.
          if (!this.disposed) this.cache.set(key, { credential, fetchedAt: this.now() });
        },
        () => {},
      );
    }
    return { credential: await awaitWithSignal(active.promise, request.signal), cacheKey: key };
  }

  private async sourceIdentity(binding: CredentialBinding): Promise<string> {
    if (binding.provider) {
      let identity = this.providerIdentities.get(binding.provider);
      if (!identity) {
        identity = this.nextProviderIdentity++;
        this.providerIdentities.set(binding.provider, identity);
      }
      return `provider:${identity}`;
    }
    const material = JSON.stringify({
      source: binding.source,
      audience: binding.audience,
      tokenUrl: binding.tokenUrl,
      clientId: binding.clientId,
      clientSecret: binding.clientSecret,
      assertionSource: binding.assertionSource,
      assertionAudience: binding.assertionAudience,
      assertion: binding.assertion,
      grantType: binding.grantType,
      parameters: binding.parameters,
      tokenRequestFormat: binding.tokenRequestFormat,
      value: binding.value,
      metadataUrl: binding.metadataUrl,
    });
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(material));
    return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('');
  }

  reject(cacheKey: string, value: string): void {
    const cached = this.cache.get(cacheKey);
    if (cached?.credential.value === value) this.cache.delete(cacheKey);
  }

  /** Terminate this application-owned cache and every in-flight acquisition. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.cache.clear();
    this.active.clear();
    this.disposeController.abort(new CredentialRegistryClosedError());
  }

  private assertOpen(): void {
    if (this.disposed) throw new CredentialRegistryClosedError();
  }

  private async acquireLeader(
    request: CredentialRequest,
    binding: CredentialBinding,
    maxDurationMs: number,
  ): Promise<Credential> {
    this.assertOpen();
    const timeout = AbortSignal.timeout(positiveDuration(maxDurationMs, DEFAULT_CREDENTIAL_TIMEOUT_MS));
    const signal = AbortSignal.any([timeout, this.disposeController.signal]);
    // A refresh can have several waiters. One caller cancellation only cancels
    // its wait; the shared provider call remains bounded by this leader timeout.
    const leaderRequest = { ...request, signal };
    try {
      const acquisition = binding.provider
        ? binding.provider(leaderRequest)
        : acquireBuiltinCredential(leaderRequest, binding);
      const credential = await awaitWithSignal(acquisition, signal);
      validateCredential(credential, this.now());
      return credential;
    } catch (error) {
      if (this.disposeController.signal.aborted) throw new CredentialRegistryClosedError();
      if (timeout.aborted && timeout.reason instanceof Error) throw timeout.reason;
      if (error instanceof ClientCredentialError) throw error;
      // The provider's own message never reaches the caller: it can carry a
      // token endpoint's response, a URL or a client id.
      throw new ClientCredentialError('credential acquisition failed');
    }
  }
}

export interface ServiceAuthOptions {
  serviceId: string;
  /** Canonical bound service URL, the default audience of a `gcp-id-token` credential. */
  serviceUrl: string;
  /** Provider contract audience, the last fallback of an OAuth credential. */
  audience: string;
  clientId: string;
  profiles: Readonly<Record<string, ClientCredentialProfile>>;
  credentials: Readonly<Record<string, CredentialBinding>>;
  manager: CredentialManager;
}

/** Provider-authored OR-of-AND authentication, evaluated on every call. */
export function serviceAuthInterceptor(options: ServiceAuthOptions): Interceptor {
  return (request, next) => authenticateServiceRequest(request, next, options);
}

/** Invalidate only credentials acquired for this exact request/source identity. */
export function rejectRequestCredentials(request: ClientRequest): void {
  const tracked = requestCredentials.get(request);
  requestCredentials.delete(request);
  for (const entry of tracked?.acquired ?? []) tracked?.manager.reject(entry.cacheKey, entry.value);
}

async function authenticateServiceRequest(
  request: ClientRequest,
  next: (req: ClientRequest) => Promise<ClientResponse>,
  options: ServiceAuthOptions,
): Promise<ClientResponse> {
  const authStarted = performance.now();
  request.headers.set(CLIENT_ID_HEADER, options.clientId);
  const acquiredServiceTokens: RejectedCredential[] = [];
  requestCredentials.delete(request);
  let forwardedUserRefresh: ((signal?: AbortSignal) => Promise<string>) | undefined;
  try {
    const policy = request.clientOperation?.security;
    if (!policy) throw new ClientCredentialError('generated operation has no security contract');
    const context = tryContext<AuthContext>();
    const selected = policy.alternatives.find((alternative) =>
      alternative.allOf.every((requirement) => isRequirementAvailable(requirement, options, context)),
    );
    if (!selected) throw new ClientCredentialError('no configured credential alternative satisfies the operation');

    for (const requirement of selected.allOf) {
      // biome-ignore lint/performance/noAwaitInLoops: AND requirements must be acquired and applied deterministically
      const applied = await applyRequirement(request, requirement, options, context);
      if (applied?.serviceToken) acquiredServiceTokens.push(applied.serviceToken);
      if (applied?.forwardedUserRefresh) forwardedUserRefresh = applied.forwardedUserRefresh;
    }
    requestCredentials.set(request, { manager: options.manager, acquired: acquiredServiceTokens });
    request.forwardedUserRefresh = forwardedUserRefresh;
  } finally {
    request.authDurationMs = performance.now() - authStarted;
  }
  try {
    return await next(request);
  } catch (error) {
    if (error instanceof ClientError && error.status === 401) {
      rejectRequestCredentials(request);
      // A forwarded-user binding that declares `refresh` gets exactly one
      // remint retry here, whether or not the operation declares 401 as a
      // typed Unauthorized error — mirroring a hand-written client's own
      // single-remint contract rather than the operation's declared retry
      // policy, which 401 does not participate in by default. Mirrors the Go
      // runtime's DoOperation (service_operation.go).
      if (forwardedUserRefresh && !request.streamedRequest) {
        const fresh = await tryForwardedUserRefresh(forwardedUserRefresh, request.signal);
        if (fresh) {
          applyForwardedUserToken(request, fresh);
          return await next(request);
        }
      }
    }
    throw error;
  }
}

/**
 * Re-mint a forwarded-user token once. A refresh that fails or returns nothing
 * yields `undefined`: the provider's refusal then stands.
 */
export async function tryForwardedUserRefresh(
  refresh: (signal?: AbortSignal) => Promise<string>,
  signal?: AbortSignal,
): Promise<string | undefined> {
  try {
    const token = (await refresh(signal)).trim();
    return token || undefined;
  } catch {
    return undefined;
  }
}

/**
 * Applies a reminted forwarded-user token directly, bypassing
 * `setCredentialHeader`'s conflicting-header guard: this intentionally
 * overwrites the stale `Authorization` value the failed attempt already set.
 */
export function applyForwardedUserToken(request: ClientRequest, token: string): void {
  const authorization = `Bearer ${token}`;
  request.headers.set('Authorization', authorization);
  request.secretValues ??= [];
  request.secretValues.push(authorization, token);
}

function isRequirementAvailable(
  requirement: ClientSecurityRequirement,
  options: ServiceAuthOptions,
  context: AuthContext | undefined,
): boolean {
  const profile = options.profiles[requirement.profile];
  if (!profile) return false;
  if (profile.kind === 'forwarded-user-token') {
    return (
      isForwardedUserSource(options.credentials[requirement.profile]?.source) && Boolean(context?.__authorizationHeader)
    );
  }
  return options.credentials[requirement.profile] !== undefined;
}

/** What one AND requirement produced: a service token to track for possible invalidation, or a forwarded-user remint hook. */
interface AppliedRequirement {
  readonly serviceToken?: RejectedCredential;
  readonly forwardedUserRefresh?: (signal?: AbortSignal) => Promise<string>;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: each declared credential kind has a distinct wire and cache behavior
async function applyRequirement(
  request: ClientRequest,
  requirement: ClientSecurityRequirement,
  options: ServiceAuthOptions,
  context: AuthContext | undefined,
): Promise<AppliedRequirement | undefined> {
  const profile = options.profiles[requirement.profile];
  if (!profile) throw new ClientCredentialError(`credential profile ${requirement.profile} is not declared`);
  if (profile.kind === 'forwarded-user-token') {
    const binding = options.credentials[requirement.profile];
    if (!isForwardedUserSource(binding?.source)) {
      throw new ClientCredentialError(`credential profile ${requirement.profile} is not enabled for forwarding`);
    }
    const authorization = context?.__authorizationHeader;
    if (!authorization) throw new ClientCredentialError('forwarded user credential is unavailable');
    setCredentialHeader(request, 'Authorization', authorization);
    recordCredential(request, requirement.profile, 'Authorization', authorization);
    return binding?.refresh ? { forwardedUserRefresh: binding.refresh } : undefined;
  }

  const binding = options.credentials[requirement.profile];
  if (!binding) throw new ClientCredentialError(`credential profile ${requirement.profile} is not configured`);
  if (profile.kind === 'api-key' || profile.kind === 'named-header') {
    if (binding.source !== 'static' || !binding.value) {
      throw new ClientCredentialError(`credential profile ${requirement.profile} requires a static value`);
    }
    setCredentialHeader(request, profile.header, binding.value);
    recordCredential(request, requirement.profile, profile.header, binding.value);
    return undefined;
  }
  if (binding.source === 'static') {
    throw new ClientCredentialError(`service-token profile ${requirement.profile} cannot use a static credential`);
  }

  const scopes = sortedUnique([...(profile.scopes ?? []), ...(requirement.scopes ?? [])]);
  const acquired = await options.manager.acquire(
    {
      serviceId: options.serviceId,
      clientId: options.clientId,
      profile: requirement.profile,
      audience: serviceTokenAudience(profile, binding, options),
      scopes,
      signal: request.signal,
    },
    binding,
    remainingDuration(request),
  );
  const credential = acquired.credential;
  setCredentialHeader(request, 'Authorization', `Bearer ${credential.value}`);
  recordCredential(request, requirement.profile, 'Authorization', `Bearer ${credential.value}`);
  return { serviceToken: { cacheKey: acquired.cacheKey, value: credential.value } };
}

/**
 * The audience one service-token acquisition asks for. The binding wins
 * because the audience is a deployment value. A Google ID token is verified
 * against the URL it is presented to (Cloud Run), which differs per environment
 * and cannot be a committed contract constant, so `gcp-id-token` defaults to
 * the binding URL and never takes the contract's audience. Every other source
 * keeps the provider's profile, then contract, audience. The Go runtime applies
 * the same rule (`serviceTokenAudience` in service_operation.go).
 */
function serviceTokenAudience(
  profile: Extract<ClientCredentialProfile, { kind: 'service-token' }>,
  binding: CredentialBinding,
  options: ServiceAuthOptions,
): string {
  const configured = binding.audience?.trim();
  if (configured) return configured;
  if (binding.source === 'gcp-id-token') return options.serviceUrl;
  return profile.audience ?? options.audience;
}

function isForwardedUserSource(source: CredentialSourceKind | undefined): boolean {
  return source === 'forwarded-user' || source === 'forwarded-user-token';
}

function recordCredential(request: ClientRequest, profile: string, header: string, value: string): void {
  request.credentialValues ??= [];
  request.credentialValues.push({ profile, value });
  request.credentialHeaderNames ??= [];
  if (!request.credentialHeaderNames.some((name) => name.toLowerCase() === header.toLowerCase())) {
    request.credentialHeaderNames.push(header);
  }
}

function setCredentialHeader(request: ClientRequest, name: string, value: string): void {
  const existing = request.headers.get(name);
  if (existing !== null && existing !== value) {
    throw new ClientCredentialError(`multiple credential requirements target header ${name}`);
  }
  request.headers.set(name, value);
  request.secretValues ??= [];
  request.secretValues.push(value, value.replace(/^Bearer\s+/i, ''));
}

async function acquireBuiltinCredential(request: CredentialRequest, binding: CredentialBinding): Promise<Credential> {
  if (binding.source === 'gcp-id-token') return fetchGcpIdToken(request, binding);
  if (binding.source === 'oauth-client-credentials') return fetchOAuthToken(request, binding);
  if (binding.source === 'oauth-extension-grant') return fetchOAuthExtensionToken(request, binding);
  if (binding.source === 'oauth-client-assertion') {
    let assertion = binding.assertion?.trim();
    if (binding.assertionSource === 'gcp-id-token') {
      assertion = (
        await fetchGcpIdToken(
          { ...request, audience: binding.assertionAudience || binding.tokenUrl || request.audience },
          binding,
        )
      ).value;
    }
    if (!assertion) throw new ClientCredentialError('client assertion is not configured');
    return fetchOAuthToken(request, binding, assertion);
  }
  throw new ClientCredentialError('service token source is not configured');
}

async function fetchOAuthExtensionToken(request: CredentialRequest, binding: CredentialBinding): Promise<Credential> {
  const endpoint = parseServiceUrl(binding.tokenUrl ?? '', {
    source: 'credential tokenUrl',
    allowInsecure: binding.allowInsecure,
  });
  const grantType = binding.grantType?.trim();
  if (!grantType?.startsWith('urn:')) {
    throw new ClientCredentialError('OAuth extension grant type must be a URN');
  }
  const clientId = binding.clientId || request.clientId;
  if (!clientId) throw new ClientCredentialError('OAuth client id is not configured');
  const reserved = ['grant_type', 'client_id', 'scope', 'audience'].find((name) =>
    Object.hasOwn(binding.parameters ?? {}, name),
  );
  if (reserved) throw new ClientCredentialError(`OAuth extension parameter ${reserved} is framework-owned`);
  const parameters: Record<string, string> = { ...(binding.parameters ?? {}) };
  parameters['grant_type'] = grantType;
  parameters['client_id'] = clientId;
  if (request.audience) parameters['audience'] = request.audience;
  if (request.scopes.length) parameters['scope'] = request.scopes.join(' ');
  const format = binding.tokenRequestFormat ?? 'form';
  const headers = new Headers({ Accept: 'application/json' });
  let body: string;
  if (format === 'json') {
    headers.set('Content-Type', 'application/json');
    body = JSON.stringify(parameters);
  } else {
    headers.set('Content-Type', 'application/x-www-form-urlencoded');
    body = new URLSearchParams(parameters).toString();
  }
  return fetchTokenResponse(endpoint, headers, body, request.signal);
}

async function fetchOAuthToken(
  request: CredentialRequest,
  binding: CredentialBinding,
  assertion?: string,
): Promise<Credential> {
  const endpoint = parseServiceUrl(binding.tokenUrl ?? '', {
    source: 'credential tokenUrl',
    allowInsecure: binding.allowInsecure,
  });
  const clientId = binding.clientId || request.clientId;
  if (!clientId) throw new ClientCredentialError('OAuth client id is not configured');
  const form = new URLSearchParams({ grant_type: 'client_credentials' });
  if (request.audience) form.set('audience', request.audience);
  if (request.scopes.length) form.set('scope', request.scopes.join(' '));
  const headers = new Headers({ Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' });
  if (assertion) {
    form.set('client_id', clientId);
    form.set('client_assertion_type', ASSERTION_TYPE);
    form.set('client_assertion', assertion);
  } else {
    if (!binding.clientSecret) throw new ClientCredentialError('OAuth client secret is not configured');
    headers.set(
      'Authorization',
      `Basic ${btoa(`${formEncodeComponent(clientId)}:${formEncodeComponent(binding.clientSecret)}`)}`,
    );
  }
  return fetchTokenResponse(endpoint, headers, form.toString(), request.signal);
}

async function fetchTokenResponse(
  endpoint: string,
  headers: Headers,
  body: string,
  signal?: AbortSignal,
): Promise<Credential> {
  const response = await fetch(endpoint, {
    method: 'POST',
    headers,
    body,
    signal,
    redirect: 'error',
  });
  const text = await readCredentialBody(response);
  if (!response.ok) throw new ClientCredentialError(`token endpoint returned status ${response.status}`);
  let payload: unknown;
  try {
    payload = JSON.parse(text);
  } catch {
    throw new ClientCredentialError('token endpoint returned invalid JSON');
  }
  if (!isRecord(payload) || typeof payload['access_token'] !== 'string' || !payload['access_token'].trim()) {
    throw new ClientCredentialError('token endpoint returned an invalid access token');
  }
  const expiresIn = parseExpiresIn(payload['expires_in']);
  return { value: payload['access_token'].trim(), expiresAt: new Date(Date.now() + expiresIn * 1000) };
}

async function fetchGcpIdToken(request: CredentialRequest, binding: CredentialBinding): Promise<Credential> {
  if (!request.audience) throw new ClientCredentialError('GCP ID token audience is empty');
  const endpoint = new URL(
    parseServiceUrl(binding.metadataUrl ?? GCP_METADATA_URL, {
      source: 'credential metadataUrl',
      allowInsecure: binding.allowInsecure ?? binding.metadataUrl === undefined,
    }),
  );
  endpoint.searchParams.set('audience', request.audience);
  endpoint.searchParams.set('format', 'full');
  const response = await fetch(endpoint, {
    headers: { 'Metadata-Flavor': 'Google' },
    signal: request.signal,
    redirect: 'error',
  });
  const token = (await readCredentialBody(response)).trim();
  if (!response.ok) throw new ClientCredentialError(`metadata server returned status ${response.status}`);
  return { value: token, expiresAt: parseJwtExpiry(token) };
}

async function readCredentialBody(response: Response): Promise<string> {
  if (!response.body) return '';
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (total <= MAX_CREDENTIAL_BODY_BYTES) {
      // biome-ignore lint/performance/noAwaitInLoops: the credential body is consumed sequentially under a hard cap
      const { done, value } = await reader.read();
      if (done) break;
      if (!value?.byteLength) continue;
      total += value.byteLength;
      if (total > MAX_CREDENTIAL_BODY_BYTES) {
        throw new ClientCredentialError('credential response exceeds maximum size');
      }
      chunks.push(value);
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
}

function formEncodeComponent(value: string): string {
  return new URLSearchParams({ value }).toString().slice('value='.length);
}

function parseExpiresIn(input: unknown): number {
  const value = typeof input === 'string' && /^\d+$/.test(input) ? Number(input) : input;
  if (!Number.isSafeInteger(value) || (value as number) <= 0) {
    throw new ClientCredentialError('token expiry is invalid');
  }
  return value as number;
}

function parseJwtExpiry(token: string): Date {
  const parts = token.split('.');
  if (parts.length !== 3 || !parts[1]) throw new ClientCredentialError('metadata server returned a malformed ID token');
  let claims: unknown;
  try {
    claims = JSON.parse(new TextDecoder().decode(Uint8Array.fromBase64(parts[1], { alphabet: 'base64url' })));
  } catch {
    throw new ClientCredentialError('metadata server returned a malformed ID token');
  }
  if (!isRecord(claims) || !Number.isSafeInteger(claims['exp'])) {
    throw new ClientCredentialError('metadata ID token has no valid expiry');
  }
  const expiry = new Date((claims['exp'] as number) * 1000);
  if (expiry.getTime() <= Date.now() + 1000) throw new ClientCredentialError('metadata ID token is expired');
  return expiry;
}

function validateCredential(credential: Credential, now: number): void {
  if (!credential || typeof credential.value !== 'string' || !credential.value.trim()) {
    throw new ClientCredentialError('credential source returned an empty value');
  }
  if (!(credential.expiresAt instanceof Date) || !Number.isFinite(credential.expiresAt.getTime())) {
    throw new ClientCredentialError('credential source returned an invalid expiry');
  }
  if (credential.expiresAt.getTime() <= now + 1000) {
    throw new ClientCredentialError('credential source returned an expired value');
  }
}

/**
 * Whether a cached credential is still usable at `now`.
 *
 * The leeway is a tenth of the credential's own lifetime, bounded to one minute
 * and floored at one second: a renewal starts before expiry, so a call never
 * carries a token that expires while it is in flight.
 */
function isFresh(cached: CachedCredential, now: number): boolean {
  const ttl = cached.credential.expiresAt.getTime() - cached.fetchedAt;
  const leeway = Math.max(1000, Math.min(60_000, Math.floor(ttl / 10)));
  return now + leeway < cached.credential.expiresAt.getTime();
}

function remainingDuration(request: ClientRequest): number {
  if (request.deadlineAt === undefined) return DEFAULT_CREDENTIAL_TIMEOUT_MS;
  return Math.max(1, request.deadlineAt - Date.now());
}

function positiveDuration(value: number, fallback: number): number {
  return Number.isFinite(value) && value > 0 ? Math.min(value, DEFAULT_CREDENTIAL_TIMEOUT_MS) : fallback;
}

function sortedUnique(values: readonly string[]): string[] {
  return [...new Set(values.filter(Boolean))].sort((left, right) => (left < right ? -1 : left > right ? 1 : 0));
}

async function awaitWithSignal<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) throw signal.reason;
  return Promise.race([
    promise,
    new Promise<never>((_resolve, reject) => {
      signal.addEventListener('abort', () => reject(signal.reason), { once: true });
    }),
  ]);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
