import type { ClientContractOperation } from '@putnami/application';
import { ClientServiceConfigError } from './errors';
import type { ClientRequest } from './transport.type';

/**
 * Receives the success body of one generated call, byte for byte.
 *
 * Some answers are proven over their exact bytes rather than trusted as a
 * decoded value: the provider derived an identifier or a digest from the byte
 * sequence it sent. Serializing the decoded value again produces other bytes —
 * another field order, other whitespace, omitted optional fields — so the
 * proof cannot be rebuilt from it. A caller that owns such a check passes a
 * sink as the generated `successBody` call option, and the ordinary generated
 * call also hands over the body it accepted.
 *
 * The body is delivered only after every check the call applies: declared
 * security and resilience, the declared status and media type, the declared
 * schema and the typed decode. A cached answer delivers the bytes the provider
 * sent when it was stored. The sink holds its own copy, so nothing a caller
 * does with it reaches the response cache.
 *
 * A sink belongs to one call at a time: a second call that reaches it while
 * the first is in flight is refused before it sends anything.
 */
export class SuccessBody {
  #claimed = false;
  #body: Uint8Array | undefined;

  /**
   * The success body the last call delivered. It is `undefined` while a call
   * holds the sink, after a call that failed, and before any call. An empty
   * body a call accepted is an empty array. The runtime keeps no reference to
   * the returned bytes.
   */
  get bytes(): Uint8Array | undefined {
    return this.#body;
  }

  /** @internal Take the sink for one call: clears it, refuses a second caller. */
  claim(): void {
    if (this.#claimed) throw new ClientServiceConfigError('a success body sink receives one call at a time');
    this.#claimed = true;
    this.#body = undefined;
  }

  /** @internal End the call that took the sink: a copy of the accepted body, or nothing on failure. */
  deliver(body: Uint8Array | undefined): void {
    this.#claimed = false;
    this.#body = body === undefined ? undefined : new Uint8Array(body);
  }

  /**
   * @internal Empty a sink a call was refused on, so a refusal delivers no
   * bytes. A sink another call holds is that call's to fill: the refusal of a
   * second caller leaves it alone.
   */
  reset(): void {
    if (!this.#claimed) this.#body = undefined;
  }
}

/**
 * Refuse a sink on an operation that declares no JSON success body to deliver
 * — not a generated unary operation, a void one, raw octets, a stream — before
 * anything is sent. Whether the transport that carries the call keeps the body
 * unchanged is the transport's own declaration, checked by the client.
 */
export function assertSuccessBodyDeliverable(
  operation: ClientContractOperation | undefined,
  operationId: string,
  successes: ClientRequest['successes'],
): void {
  if (operation?.stream !== 'unary') {
    throw new ClientServiceConfigError(
      `operation ${operationId} is not a generated unary operation; only one delivers its success body`,
    );
  }
  let declared = false;
  for (const success of successes ?? []) {
    for (const content of success.content) {
      if (content.streamed || (content.schema?.type === 'string' && content.schema.format === 'binary')) {
        throw new ClientServiceConfigError(
          `operation ${operationId} declares a raw octet success payload; its generated method returns the octets unchanged`,
        );
      }
      declared = true;
    }
  }
  if (!declared) throw new ClientServiceConfigError(`operation ${operationId} declares no success body to deliver`);
}
