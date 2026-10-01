/**
 * Raw octet payload declarations.
 *
 * A first-party contract has one representation for bytes inside a JSON
 * document — base64, `format: byte` — and it is the wrong one for a body that
 *is* bytes: it grows the payload by a third, and it hides the size behind an
 * encoding. `Binary` declares the other case: the HTTP body itself is octets,
 * under a named media type, bounded.
 *
 * `BinaryStream` instead transfers ownership of a readable stream, with the
 * concrete content type supplied at runtime. It never buffers the full body.
 */

/** The marker `.body()` and `.returns()` accept for a raw octet payload. */
export interface BinarySchema {
  readonly __putnamiBinary: true;
  readonly mediaType: string;
  readonly maxBytes: number;
  readonly streamed?: boolean;
}

/** A raw, unframed HTTP body whose reader belongs to the handler. */
export interface BinaryStreamSchema extends BinarySchema {
  readonly streamed: true;
}

/** Declare a bounded raw HTTP stream; this is independent of message/WebSocket streams. */
export function BinaryStream(options: { maxBytes: number }): BinaryStreamSchema {
  if (!Number.isSafeInteger(options.maxBytes) || options.maxBytes <= 0) {
    throw new Error(`BinaryStream needs a strictly positive byte bound, got ${options.maxBytes}`);
  }
  return { __putnamiBinary: true, mediaType: '*/*', maxBytes: options.maxBytes, streamed: true };
}

/**
 * The octets a declared binary body delivers to a handler.
 *
 * The buffer type is named rather than left open: under TypeScript's typed
 * ArrayBuffer views a `Uint8Array<ArrayBufferLike>` is not assignable to
 * `BufferSource`, so a handler could not return its own request payload
 * without copying it.
 */
export type BinaryBody = Uint8Array<ArrayBuffer>;

/** The two facts a raw octet declaration carries to every consumer. */
export interface BinaryMeta {
  readonly mediaType: string;
  readonly maxBytes: number;
  readonly streamed?: boolean;
}

/** Validate a concrete HTTP media type without changing its spelling or parameters. */
export function isConcreteBinaryContentType(value: string | null | undefined): boolean {
  if (!value || /[\r\n]/.test(value)) return false;
  const token = "[!#$%&'+.^_`|~0-9A-Za-z-]+";
  const parameterToken = "[!#$%&'*+.^_`|~0-9A-Za-z-]+";
  const quoted = '"(?:[\\t !#-\\[\\]-~]|\\\\[\\t !-~])*"';
  const parameter = `[ \\t]*;[ \\t]*(${parameterToken})[ \\t]*=[ \\t]*(?:${parameterToken}|${quoted})`;
  if (!new RegExp(`^[ \\t]*${token}/${token}(?:${parameter})*[ \\t]*$`).test(value)) return false;
  const names = new Set<string>();
  for (const match of value.matchAll(new RegExp(parameter, 'g'))) {
    const name = match[1].toLowerCase();
    if (names.has(name)) return false;
    names.add(name);
  }
  return true;
}

/** Refuse malformed markers before they reach either the runtime or OpenAPI. */
export function validateBinaryMeta(value: BinaryMeta): void {
  if (value.streamed === true) {
    if (value.mediaType !== '*/*' || !Number.isSafeInteger(value.maxBytes) || value.maxBytes <= 0) {
      throw new Error('BinaryStream requires wildcard media type and a positive byte bound');
    }
  } else {
    if (value.streamed !== undefined) throw new Error('Binary streamed marker must be true or absent');
    Binary(value);
  }
}

/** The media type a raw octet payload takes when the author names none. */
export const DEFAULT_BINARY_MEDIA_TYPE = 'application/octet-stream';

/**
 * Declare a raw octet request body or success response.
 *
 * ```typescript
 * endpoint()
 *   .body(Binary({ maxBytes: 65_536 }))
 *   .returns({ size: Int })
 *   .handle(async (ctx) => ({ size: (await ctx.body<Uint8Array>())?.byteLength ?? 0 }));
 * ```
 *
 * It throws on a media type that is empty or JSON-structured, and on a bound
 * that is not a positive integer: all three are authoring mistakes that would
 * otherwise reach the published contract.
 */
export function Binary(options: { mediaType?: string; maxBytes: number }): BinarySchema {
  const mediaType = (options.mediaType ?? DEFAULT_BINARY_MEDIA_TYPE).trim();
  if (!mediaType || mediaType.includes(';') || !/^[\w.+-]+\/[\w.+-]+$/.test(mediaType)) {
    throw new Error(`Binary: ${JSON.stringify(options.mediaType)} is not a media type`);
  }
  if (isJsonMediaType(mediaType)) {
    throw new Error(
      `Binary: ${JSON.stringify(mediaType)} is a JSON media type; declare a JSON body with a schema instead`,
    );
  }
  if (!Number.isSafeInteger(options.maxBytes) || options.maxBytes <= 0) {
    throw new Error(
      `Binary: ${JSON.stringify(mediaType)} needs a strictly positive byte bound, got ${options.maxBytes}`,
    );
  }
  return { __putnamiBinary: true, mediaType, maxBytes: options.maxBytes };
}

/** Narrow a `.body()` / `.returns()` argument to a raw octet declaration. */
export function isBinarySchema(value: unknown): value is BinarySchema {
  return typeof value === 'object' && value !== null && (value as Partial<BinarySchema>).__putnamiBinary === true;
}

/** The media types the JSON pipeline owns: `application/json` and every `+json` suffix. */
function isJsonMediaType(mediaType: string): boolean {
  const lower = mediaType.toLowerCase();
  return lower === 'application/json' || lower.endsWith('+json');
}

/** Strip the parameters a `Content-Type` may carry (`; charset=…`). */
export function baseMediaType(header: string | null | undefined): string {
  return (header ?? '').split(';', 1)[0]?.trim().toLowerCase() ?? '';
}
