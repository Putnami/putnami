/**
 * Provider-owned WebSocket wires.
 *
 * A first-party stream travels in the `putnami.service.v1` conversation: JSON
 * envelope frames, admission in the first client frame. Two shapes cannot:
 *
 * - a **byte stream** — raw octets in binary messages both ways, declared with
 *   `.body(ByteStream()).returns(ByteStream())`;
 * - a **provider-owned subprotocol** — one JSON value of the declared message
 *   types per text message, under the token `.subprotocol()` names.
 *
 * Both admit on the upgrade request: the endpoint's middleware and validation
 * run on it, exactly as they do for SSE, and the provider's own protocol owns
 * everything after the handshake. `protocols/clientcontract` ADR 0010 records
 * the decision.
 */

/** The marker `.body()` and `.returns()` accept for a byte stream. */
export interface ByteStreamSchema {
  readonly __putnamiByteStream: true;
}

/**
 * One run of octets a byte stream carries. Message boundaries mean nothing on
 * a byte stream, so a handler must not rely on how the peer split its writes.
 */
export type ByteChunk = Uint8Array<ArrayBuffer>;

/** The provider-owned wire an endpoint declares. */
export interface ProviderWire {
  /** The negotiated token. Absent only on a byte stream that negotiates none. */
  readonly subprotocol?: string;
  /** Raw octets in binary messages, rather than JSON values in text messages. */
  readonly bytes: boolean;
}

/** The first-party conversation's namespace, which a provider-owned wire never declares. */
export const RESERVED_SUBPROTOCOL_PREFIX = 'putnami.service.';

const SUBPROTOCOL_TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

/**
 * Declare raw octets in binary WebSocket messages. A byte stream carries octets
 * both ways, so an endpoint declares it on both sides:
 *
 * ```typescript
 * endpoint()
 *   .query({ database: String })
 *   .body(ByteStream())
 *   .returns(ByteStream())
 *   .secure({ scopes: ['gateway:connect'] })
 *   .handle(async (ctx) => {
 *     for await (const chunk of ctx.messages()) ctx.send(chunk);
 *   });
 * ```
 */
export function ByteStream(): ByteStreamSchema {
  return { __putnamiByteStream: true };
}

/** Narrow a `.body()` / `.returns()` argument to a byte stream declaration. */
export function isByteStreamSchema(value: unknown): value is ByteStreamSchema {
  return (
    typeof value === 'object' && value !== null && (value as Partial<ByteStreamSchema>).__putnamiByteStream === true
  );
}

/**
 * Whether a value is a negotiable RFC 6455 subprotocol: a non-empty RFC 9110
 * token, which cannot carry a second token, whitespace or a header.
 */
export function isWebSocketSubprotocolToken(value: string): boolean {
  return SUBPROTOCOL_TOKEN.test(value);
}
