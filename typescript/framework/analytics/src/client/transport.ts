import type { WireBatch, WireEvent } from './wire';

/** The only `protocolVersion` the ingest route accepts. */
export const PROTOCOL_VERSION = 1;

/** What the caller must do with the batch it just handed over. */
type SendOutcome = 'ok' | 'drop' | 'retry' | 'pending';

/** Serializes a batch exactly as the ingest route parses it (body §A.1). */
export function encodeBatch(events: WireEvent[], now: number = Date.now()): string {
  const batch: WireBatch = {
    protocolVersion: PROTOCOL_VERSION,
    sentAt: new Date(now).toISOString(),
    events,
  };
  return JSON.stringify(batch);
}

function outcomeOf(status: number): SendOutcome {
  if (status >= 200 && status < 300) {
    return 'ok';
  }
  // 429 and every 5xx are "come back later"; any other 4xx is a batch the
  // server has already decided it will never take, so retrying it forever
  // would only burn the visitor's battery.
  return status === 429 || status >= 500 ? 'retry' : 'drop';
}

/**
 * Ships one batch to the ingest route (body §D.15).
 *
 * Two transports, chosen by whether the page is going away:
 *
 * - On unload, `navigator.sendBeacon` with a `text/plain` blob. `text/plain`
 *   is a CORS-safelisted content type, so the beacon is a simple request and
 *   never triggers a preflight the closing document could not complete. The
 *   result confirms only that the browser queued the payload, not that the
 *   server received it. The caller therefore keeps the durable events for a
 *   later response-confirmed retry; stable ids make a duplicate harmless.
 * - Otherwise `fetch` with `keepalive: true` (so an in-flight batch survives a
 *   navigation) and `credentials: 'same-origin'` (the route is same-origin and
 *   unauthenticated; sending credentials cross-origin would be a leak).
 *
 * A `sendBeacon` returning `false` — payload above the browser's beacon
 * budget, or the beacon queue full — falls through to `fetch`.
 *
 * @param endpoint - The ingest path from the bootstrap.
 * @param events - The batch, already capped at the wire ceiling.
 * @param opts - `unload` selects the beacon transport.
 * @returns Whether to acknowledge, drop, retry, or retain a pending beacon.
 */
export async function send(endpoint: string, events: WireEvent[], opts: { unload: boolean }): Promise<SendOutcome> {
  const body = encodeBatch(events);

  if (opts.unload) {
    const beacon = (globalThis as { navigator?: Navigator }).navigator?.sendBeacon;
    if (typeof beacon === 'function' && beacon.call(navigator, endpoint, new Blob([body], { type: 'text/plain' }))) {
      return 'pending';
    }
  }

  try {
    const response = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
      keepalive: true,
      credentials: 'same-origin',
    });
    return outcomeOf(response.status);
  } catch {
    // A network error is indistinguishable from a server the visitor cannot
    // reach yet. Keep the batch; the ids make a later success idempotent.
    return 'retry';
  }
}
