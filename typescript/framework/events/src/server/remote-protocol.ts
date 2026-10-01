import type { HandlerDefinition } from '../handler/handler';
import type { AttributeFilter, ResolvedHandlerOptions } from '../handler/handler.type';
import type { Envelope } from '../transport/transport';

// ---------------------------------------------------------------------------
// Remote events HTTP protocol — request bodies and validation
//
// The wire shapes exchanged with the local events server and the validators
// that guard each endpoint, kept separate from the HTTP routing in
// `memory-server.ts`.
// ---------------------------------------------------------------------------

export interface RemoteSubscribeBody {
  topic: string;
  options: ResolvedHandlerOptions;
  filter?: AttributeFilter;
}

export interface RemotePullBody {
  subscriberId: string;
  timeoutMs?: number;
}

export interface RemoteAckBody {
  subscriberId: string;
  deliveryId: string;
  status: 'ack' | 'nack';
  reason?: string;
}

export interface RemoteUnsubscribeBody {
  subscriberId: string;
}

/** Validate the full envelope structure (not just topic + id) before publishing. */
export function isValidEnvelope(envelope: Envelope): boolean {
  return (
    typeof envelope.topic === 'string' &&
    typeof envelope.id === 'string' &&
    typeof envelope.attempt === 'number' &&
    envelope.attempt >= 1 &&
    typeof envelope.timestamp === 'string' &&
    typeof envelope.attributes === 'object' &&
    envelope.attributes !== null &&
    !Array.isArray(envelope.attributes)
  );
}

/**
 * Strip caller-supplied reserved attributes from a remotely-published envelope.
 *
 * `auth.*` attributes are server-captured identity: the in-process publisher
 * (see `buildEnvelope`) auto-captures them from the authenticated context and
 * strips any caller-supplied `auth.*` so callers "cannot override them". The
 * remote `/publish` endpoint bypasses that guard — a holder of the shared bearer
 * token could otherwise POST an envelope with forged `auth.*` that downstream
 * handlers read as authenticated identity (cross-service spoofing). The local
 * dev server cannot verify the end-user behind a shared token, so it drops every
 * caller-supplied `auth.*` rather than trusting it — failing safe to *no*
 * identity, never a *forged* one.
 *
 * Also coerces the attribute bag to its declared `Record<string, string>` shape
 * by dropping non-string values, since the wire payload is untrusted JSON.
 *
 * Mutates and returns the envelope.
 */
export function sanitizeRemoteEnvelope(envelope: Envelope): Envelope {
  const sanitized: Record<string, string> = {};
  for (const [key, value] of Object.entries(envelope.attributes)) {
    if (key.startsWith('auth.')) continue;
    if (typeof value !== 'string') continue;
    sanitized[key] = value;
  }
  envelope.attributes = sanitized;
  return envelope;
}

export function isValidSubscribeBody(body: Partial<RemoteSubscribeBody>): body is RemoteSubscribeBody {
  if (typeof body.topic !== 'string' || body.topic.length === 0) {
    return false;
  }

  const options = body.options;
  if (!options) {
    return false;
  }

  if (
    (options.distribution !== 'broadcast' && options.distribution !== 'competing') ||
    typeof options.maxRetries !== 'number' ||
    typeof options.maxBackoff !== 'number' ||
    typeof options.timeout !== 'number' ||
    typeof options.concurrency !== 'number' ||
    typeof options.queueLimit !== 'number' ||
    (options.overflow !== 'throw' && options.overflow !== 'drop') ||
    typeof options.dlq !== 'boolean' ||
    (options.ack !== 'auto' && options.ack !== 'manual')
  ) {
    return false;
  }

  if (body.filter === undefined) {
    return true;
  }

  if (typeof body.filter !== 'object' || body.filter === null) {
    return false;
  }

  if (typeof body.filter.attributes !== 'object' || body.filter.attributes === null) {
    return false;
  }

  return Object.values(body.filter.attributes).every((value) => typeof value === 'string');
}

/**
 * Build a synthetic handler definition for a remote subscription.
 *
 * The remote subscribe wire body carries only the topic *name* — the topic's
 * payload schema is owned by the consuming service and never travels to this
 * server, which wraps a schema-agnostic broker. The synthetic topic therefore
 * declares an empty schema, and server-side `assertValidPayload` is a
 * deliberate no-op for the cross-service path: the local dev server trusts
 * publishers and defers payload validation to the consumer.
 *
 * The owning `LocalServerTransport` re-validates every pulled delivery against
 * the real topic schema before invoking the handler (see
 * `local-server.transport.ts`), so a malformed payload is still rejected before
 * any handler runs, at the far consumer rather than at this relay. The
 * documented "validated before dispatch" guarantee is consumer-side for remote
 * subscriptions.
 */
export function toHandlerDefinition(body: RemoteSubscribeBody): HandlerDefinition {
  return {
    __handler: 'putnami:event-handler',
    topic: {
      __topic: 'putnami:topic',
      name: body.topic,
      schema: {},
    },
    options: body.options,
    filter: body.filter,
    handler: async () => {},
  };
}
