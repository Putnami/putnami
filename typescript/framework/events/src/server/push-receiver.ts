import {
  type HttpRequestContext,
  type HttpResponse,
  badRequest,
  forbidden,
  internalServerError,
  json,
  noContent,
  unauthorized,
  useOAuthService,
} from '@putnami/application';
import type { DetachedScope } from '@putnami/runtime';
import type { HandlerDefinition } from '../handler/handler';
import type { AttributeFilter } from '../handler/handler.type';
import { PUTNAMI_EVENTS_PROTOCOL } from '../protocol';
import { dispatchToHandler } from '../transport/dispatch';
import { createTransportMessage } from '../transport/message-utils';
import type { Envelope } from '../transport/transport';
import { isValidEnvelope, sanitizeRemoteEnvelope } from './remote-protocol';

// ---------------------------------------------------------------------------
// Push delivery receiver — events as webhooks (serverless)
//
// In push delivery the provider POSTs each event to this route instead of the
// workload holding a pull/stream loop. The receiver verifies the pusher's OIDC
// token fail-closed (signature/issuer/audience + a service-account allowlist),
// unwraps the provider push body, sanitizes and validates the canonical
// envelope, dispatches it through the shared per-delivery pipeline, and maps the
// outcome to an HTTP status: 2xx ack, 4xx dead-letter, 5xx retry. Push delivery
// is at-least-once, so handlers must be idempotent.
// ---------------------------------------------------------------------------

/** Delivery model for a subscription. */
export type Delivery = 'pull' | 'stream' | 'push';

/**
 * Conventional receiver route, in the app router's `[param]` syntax. Matches the
 * canonical convention `/_putnami/events/:subscription`; the placeholder syntax
 * is router-specific, the matched URLs are identical across frameworks.
 */
export const PUSH_RECEIVER_PATH = '/_putnami/events/[subscription]';

/** Maximum push body size; an oversized push is permanently rejected (DLQ). */
const MAX_PUSH_BYTES = 1_048_576;

/** Verified OIDC claims, or undefined when verification fails. */
export type PushTokenVerifier = (token: string) => Promise<Record<string, unknown> | undefined>;

interface PushConfigBase {
  /** OIDC issuer of the pusher token (e.g. "https://accounts.google.com"). */
  issuer?: string;
  /** Audience the pusher token must carry (the receiver endpoint URL). */
  audience?: string;
  /** Allowlist of pusher service-account emails. Empty rejects everything. */
  allowedServiceAccounts?: string[];
  /**
   * Override the OIDC verifier. Defaults to the app `OAuthService.verify()`
   * with the configured issuer + audience — provide a custom verifier when the
   * pusher's issuer/JWKS differs from the app's own IdP.
   */
  verify?: PushTokenVerifier;
}

/** Normal enabled push receiver configuration; enabled defaults to true. */
export interface EnabledPushConfig extends PushConfigBase {
  enabled?: true;
  issuer: string;
  audience: string;
  allowedServiceAccounts: string[];
}

/** First-deploy bootstrap mode: mount the route but reject before auth or dispatch. */
export interface DisabledPushConfig extends PushConfigBase {
  enabled: false;
}

export type PushConfig = EnabledPushConfig | DisabledPushConfig;

/** The Pub/Sub push wrapper POSTed to the receiver (untrusted JSON). */
interface PushWrapper {
  message?: {
    data?: string;
    attributes?: Record<string, string>;
    messageId?: string;
    publishTime?: string;
    orderingKey?: string;
  };
  subscription?: string;
}

/**
 * Build the secured push receiver route handler. Authentication runs first and
 * fail-closed; only an authenticated, allowlisted pusher reaches dispatch.
 */
export function createPushReceiver(
  config: PushConfig,
  handlers: HandlerDefinition[],
  scopeFactory?: () => Promise<DetachedScope | undefined>,
): (ctx: HttpRequestContext) => Promise<HttpResponse> {
  if (config.enabled === false) {
    return async () => json({ error: 'event push receiver disabled' }, { status: 503 });
  }
  const enabledConfig = requireEnabledConfig(config);
  const allowlist = new Set(enabledConfig.allowedServiceAccounts);
  const verify = enabledConfig.verify ?? defaultVerifier(enabledConfig);
  // Per-topic round-robin counters for competing distribution (matches the broker).
  const roundRobin = new Map<string, number>();

  return async (ctx) => {
    const token = bearerToken(ctx.headers);
    if (!token) {
      return unauthorized();
    }
    const claims = await verify(token);
    if (!claims) {
      return unauthorized();
    }
    const email = typeof claims['email'] === 'string' ? (claims['email'] as string) : undefined;
    if (!email || claims['email_verified'] === false || !allowlist.has(email)) {
      return forbidden();
    }

    const contentLength = Number(ctx.headers.get('content-length') ?? '0');
    if (Number.isFinite(contentLength) && contentLength > MAX_PUSH_BYTES) {
      return json({ error: 'payload too large' }, { status: 413 });
    }

    // A malformed body throws in ctx.body(); per the push contract that is a
    // permanent (4xx/DLQ) condition, not a 5xx/retry, so catch it here.
    let wrapper: PushWrapper | undefined;
    try {
      wrapper = await ctx.body<PushWrapper>();
    } catch {
      return badRequest({ error: 'invalid push body' });
    }
    const envelope = decodePushEnvelope(wrapper);
    if (!envelope) {
      return badRequest({ error: 'invalid push envelope' });
    }

    return dispatch(envelope, handlers, roundRobin, scopeFactory);
  };
}

/** The default OIDC verifier reuses the app OAuthService for the push issuer. */
function requireEnabledConfig(config: PushConfig): EnabledPushConfig {
  if (!config.issuer || !config.audience || !config.allowedServiceAccounts) {
    throw new Error('enabled events push delivery requires issuer, audience, and allowedServiceAccounts');
  }
  return config as EnabledPushConfig;
}

function defaultVerifier(config: EnabledPushConfig): PushTokenVerifier {
  return (token) =>
    useOAuthService().verify<Record<string, unknown>>(token, {
      issuer: config.issuer,
      audience: config.audience,
    });
}

function bearerToken(headers: Headers): string | undefined {
  const authorization = headers.get('authorization');
  if (!authorization?.startsWith('Bearer ')) {
    return undefined;
  }
  return authorization.slice('Bearer '.length).trim() || undefined;
}

/**
 * Unwrap a provider push body into the canonical envelope, applying the same
 * field defaults the Google Pub/Sub transport applies and stripping caller-
 * supplied `auth.*` attributes (anti-spoofing: an external pusher must not be
 * able to forge a carried end-user identity). Returns undefined on any malformed
 * input so the receiver can dead-letter it.
 */
export function decodePushEnvelope(wrapper: PushWrapper | undefined): Envelope | undefined {
  // The push wrapper contract requires a subscription identifier; reject a
  // wrapper that omits it even when the encoded envelope is otherwise valid.
  if (typeof wrapper?.subscription !== 'string' || wrapper.subscription.length === 0) {
    return undefined;
  }
  const data = wrapper?.message?.data;
  if (typeof data !== 'string' || data.length === 0) {
    return undefined;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(Buffer.from(data, 'base64').toString('utf8'));
  } catch {
    return undefined;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return undefined;
  }

  const envelope = parsed as Envelope;
  envelope.protocol = envelope.protocol ?? PUTNAMI_EVENTS_PROTOCOL;
  if (!envelope.id) {
    envelope.id = wrapper?.message?.messageId ?? '';
  }
  if (!envelope.timestamp) {
    envelope.timestamp = new Date().toISOString();
  }
  if (typeof envelope.attempt !== 'number' || envelope.attempt < 1) {
    envelope.attempt = 1;
  }
  if (!envelope.attributes || typeof envelope.attributes !== 'object') {
    envelope.attributes = wrapper?.message?.attributes ?? {};
  }

  sanitizeRemoteEnvelope(envelope);
  if (!isValidEnvelope(envelope)) {
    return undefined;
  }
  return envelope;
}

/**
 * Dispatch the envelope to the selected handlers and map the outcome to an HTTP
 * status. A handler failure returns 5xx (provider retries); a delivery with no
 * matching handler is acknowledged and dropped.
 */
async function dispatch(
  envelope: Envelope,
  handlers: HandlerDefinition[],
  roundRobin: Map<string, number>,
  scopeFactory?: () => Promise<DetachedScope | undefined>,
): Promise<HttpResponse> {
  for (const def of selectPushTargets(envelope, handlers, roundRobin)) {
    const { message, ackState, abortController } = createTransportMessage(envelope, def.options);
    try {
      await dispatchToHandler(
        message,
        ackState,
        abortController,
        envelope,
        def,
        async (msg) => {
          await def.handler(msg);
        },
        { scopeFactory },
      );
    } catch {
      return internalServerError({ error: 'handler failed' });
    }
  }
  return noContent();
}

/**
 * Apply the same distribution semantics as the broker (memory-broker.ts
 * selectTargets): every matching broadcast handler receives the event, and
 * competing handlers share deliveries round-robin (one per message). Without
 * this, push delivery would invoke every competing handler and duplicate side
 * effects.
 */
function selectPushTargets(
  envelope: Envelope,
  handlers: HandlerDefinition[],
  roundRobin: Map<string, number>,
): HandlerDefinition[] {
  const matching = handlers.filter(
    (def) => def.topic.name === envelope.topic && filterMatches(def.filter, envelope.attributes),
  );
  const broadcast = matching.filter((def) => def.options.distribution === 'broadcast');
  const competing = matching.filter((def) => def.options.distribution === 'competing');

  const targets: HandlerDefinition[] = [...broadcast];
  if (competing.length > 0) {
    const idx = roundRobin.get(envelope.topic) ?? 0;
    targets.push(competing[idx % competing.length]);
    roundRobin.set(envelope.topic, (idx + 1) % competing.length);
  }
  return targets;
}

function filterMatches(filter: AttributeFilter | undefined, attributes: Record<string, string>): boolean {
  if (!filter?.attributes) {
    return true;
  }
  return Object.entries(filter.attributes).every(([key, value]) => attributes[key] === value);
}
