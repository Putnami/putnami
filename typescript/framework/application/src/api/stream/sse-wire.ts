/**
 * The negotiated SSE wire of client contract protocol v1 (ADR 0013 of
 * protocols/clientcontract). A consumer asks for it only on an operation whose
 * SSE transport declares a continuation, and a provider grants it only on a
 * route that declares one. Everything else keeps the unnegotiated framing, byte
 * for byte. Mirrors `sse.go` in protocols/clientcontract.
 */

/** Request and response header that negotiates the wire. */
export const SSE_WIRE_HEADER = 'X-Putnami-Stream-Wire';

/** The only negotiated SSE wire of protocol v1. */
export const SSE_WIRE_V1 = 'putnami.sse.v1';

/** The exact bytes a provider writes to end a negotiated stream successfully. */
export const SSE_COMPLETE_FRAME = 'event: complete\ndata: {}\n\n';

/**
 * Whether a header value names exactly the negotiated wire. Pass the value
 * `Headers.get` returns: it joins repeated field lines with `", "` the way RFC
 * 9110 combines them, so a list, a repeated line, another version or another
 * letter case never negotiates. Optional whitespace is trimmed.
 */
export function negotiatesSseWire(value: string | null | undefined): boolean {
  return typeof value === 'string' && value.replace(/^[ \t]+|[ \t]+$/g, '') === SSE_WIRE_V1;
}

/** The default event type: a block without an event field, or one naming it, carries a message. */
export const SSE_EVENT_MESSAGE = 'message';

/** The typed terminal error event. Both wires carry it, with the error envelope plus the status. */
export const SSE_EVENT_ERROR = 'error';

/** The successful terminal event of the negotiated wire. A control event, never a message. */
export const SSE_EVENT_COMPLETE = 'complete';

/** The fixed control payload of the complete event. */
export const SSE_COMPLETE_DATA = '{}';

/** What one dispatched event means to a reader. */
export type SseEventKind = 'message' | 'error' | 'complete';

/**
 * Name one dispatched event from its event field (empty when the block has
 * none) and its data lines joined with `\n`. Mirrors `ClassifySSEEvent`.
 *
 * Without negotiation it keeps the default reading both runtimes share:
 * `error` is the terminal error and every other type is an application
 * message. The negotiated wire has a closed vocabulary: the default type is a
 * message, `error` is the terminal error, `complete` carrying exactly
 * {@link SSE_COMPLETE_DATA} is the successful terminal, and any other event
 * is a contract error, reported as the thrown `Error`.
 */
export function classifySseEvent(eventType: string, data: string, negotiated: boolean): SseEventKind {
  if (eventType === SSE_EVENT_ERROR) return 'error';
  if (!negotiated) return 'message';
  switch (eventType) {
    case '':
    case SSE_EVENT_MESSAGE:
      return 'message';
    case SSE_EVENT_COMPLETE:
      if (data !== SSE_COMPLETE_DATA) {
        throw new Error(
          `the ${SSE_EVENT_COMPLETE} event carries ${JSON.stringify(data)}; ${SSE_WIRE_V1} fixes its payload to ${JSON.stringify(SSE_COMPLETE_DATA)}`,
        );
      }
      return 'complete';
    default:
      throw new Error(`event type ${JSON.stringify(eventType)} is not part of ${SSE_WIRE_V1}`);
  }
}

/**
 * The position one output message carries in the declared field: a non-empty
 * JSON string, returned exactly as decoded. Mirrors `SSECursorValue`. A
 * message without it, or with any other value, is a contract error (thrown): a
 * stream that cannot say where it is cannot be continued.
 */
export function sseCursorValue(message: unknown, outputField: string): string {
  if (typeof message !== 'object' || message === null || Array.isArray(message)) {
    throw new Error('the output message is not a JSON object');
  }
  if (!Object.hasOwn(message, outputField)) {
    throw new Error(`the output message carries no ${JSON.stringify(outputField)} position`);
  }
  const value = (message as Record<string, unknown>)[outputField];
  if (typeof value !== 'string')
    throw new Error(`the output field ${JSON.stringify(outputField)} is not a JSON string`);
  if (value === '') throw new Error(`the output field ${JSON.stringify(outputField)} is empty`);
  return value;
}

/** The query one SSE opening sends: each parameter with one value or several. */
export type SseQuery = Readonly<Record<string, string | readonly string[] | undefined>>;

/** One continuation declaration as the reopen rule reads it. */
export type SseReopenContinuation =
  | { readonly mode: 'cursor'; readonly cursor: { readonly queryParameter: string } }
  | { readonly mode: 'best-effort' };

/**
 * The query a reopened connection sends, as a new value; `original` is never
 * modified. Mirrors `SSEReopenQuery`.
 *
 * Best-effort continuation reopens with the original query. Cursor mode
 * replaces every value of the declared parameter with `delivered`, the
 * position of the last message the consumer received. Until the consumer has
 * received one (`delivered` is empty) it keeps the original query, including
 * an initial position the caller supplied. No mode ever synthesizes a position.
 */
export function sseReopenQuery(
  continuation: SseReopenContinuation,
  original: SseQuery | undefined,
  delivered: string,
): Record<string, string | readonly string[] | undefined> {
  const query: Record<string, string | readonly string[] | undefined> = {};
  for (const [name, value] of Object.entries(original ?? {})) {
    query[name] = typeof value === 'string' || value === undefined ? value : [...value];
  }
  if (continuation.mode !== 'cursor' || delivered === '') return query;
  query[continuation.cursor.queryParameter] = delivered;
  return query;
}
