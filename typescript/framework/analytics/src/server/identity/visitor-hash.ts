import { createHmac } from 'node:crypto';

/** Domain separator of the daily key, versioned so a rotation is expressible. */
const DAY_KEY_LABEL = 'putnami-analytics/v1/';

/** The number of base64url characters kept from the visitor digest. */
export const VISITOR_ID_LENGTH = 22;

/** What the daily visitor hash consumes. The IP never leaves this call. */
export interface VisitorHashInput {
  /** The server-side key (`analytics.secret` or `session.cookieSecret`). */
  secret: string;
  /** The event time; only its UTC day is used. */
  ts: Date;
  /** The application name, so two apps sharing a secret cannot correlate. */
  app: string;
  /** The client IP, used and discarded. */
  clientIp: string;
  /** `browser/os` from the classifier — never the raw User-Agent. */
  uaFamily: string;
}

/**
 * The UTC day of a timestamp, as `YYYY-MM-DD`.
 *
 * @param ts - The instant to bucket.
 * @returns The UTC calendar day.
 */
export function utcDay(ts: Date): string {
  return ts.toISOString().slice(0, 10);
}

/**
 * Derives the key of one UTC day from the server secret.
 *
 * @param secret - The server-side key.
 * @param day - The UTC day, as {@link utcDay} renders it.
 * @returns The 32-byte day key.
 */
export function dayKey(secret: string, day: string): Buffer {
  return createHmac('sha256', secret).update(`${DAY_KEY_LABEL}${day}`).digest();
}

/**
 * Computes the cookieless visitor id (body §D.1).
 *
 * The key rotates at UTC midnight, so the same visitor yields unrelated ids on
 * different days *by construction* rather than by a deletion policy — which is
 * what makes the measurement exempt from consent. The IP address exists only
 * inside this function: it is never returned, never logged, never stored.
 *
 * @param input - The secret, the event time, and the request's identifying triple.
 * @returns A 22-character base64url visitor id.
 */
export function visitorHash(input: VisitorHashInput): string {
  const key = dayKey(input.secret, utcDay(input.ts));
  return createHmac('sha256', key)
    .update(`${input.app}\n${input.clientIp}\n${input.uaFamily}`)
    .digest('base64url')
    .slice(0, VISITOR_ID_LENGTH);
}
