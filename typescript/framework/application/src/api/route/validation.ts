import type { AotFieldValidator, AotValidators, SchemaDefinition } from '@putnami/runtime';
import { PayloadTooLargeException, UnsupportedMediaTypeException } from '@putnami/runtime';
import type { HttpRequestContext } from '../../http/http-context.type';
import { baseMediaType, type BinaryBody, type BinaryMeta, isConcreteBinaryContentType } from './binary';
import type { BodyContentType } from './endpoint.types';
import { validateSchema } from './validate';

/**
 * Shared, AOT-capable input validation — used by the API endpoint, web
 * loader/action, and (via {@link runValidator}) the stream wrappers.
 *
 * Each schema is validated by its compiled (AOT) validator when one was emitted
 * at build time, otherwise by the generic `validateSchema`. AOT validators
 * delegate any anomaly back to `validateSchema`, so behaviour and error messages
 * are identical either way.
 */

export interface ValidationSchemas {
  readonly params?: SchemaDefinition;
  readonly query?: SchemaDefinition;
  readonly body?: SchemaDefinition;
  readonly bodyContentType?: BodyContentType;
  readonly bodyBinary?: BinaryMeta;
  readonly headers?: SchemaDefinition;
}

/** Run a compiled (AOT) validator when present, otherwise the generic one. */
export function runValidator(
  schema: SchemaDefinition,
  raw: unknown,
  options: { coerce: boolean; label: string },
  aot?: AotFieldValidator,
): Record<string, unknown> {
  if (aot) {
    return aot(raw, (r) => validateSchema(schema, r, options));
  }
  return validateSchema(schema, raw, options);
}

/**
 * Validate and replace `ctx.params` / `ctx.queryParams` / `ctx.body` in place,
 * per the declared schemas, using compiled validators when available.
 */
export function applyInputValidation(
  ctx: HttpRequestContext,
  schemas: ValidationSchemas | undefined,
  aot?: AotValidators,
): void {
  if (schemas?.params) {
    ctx.params = runValidator(
      schemas.params,
      ctx.params ?? {},
      { coerce: true, label: 'params' },
      aot?.params,
    ) as Record<string, string>;
  }

  if (schemas?.query) {
    const rawQuery = ctx.queryParams();
    const validatedQuery = runValidator(schemas.query, rawQuery, { coerce: true, label: 'query' }, aot?.query);
    ctx.queryParams = () => validatedQuery as Record<string, string>;
  }

  if (schemas?.bodyBinary) {
    const declared = schemas.bodyBinary;
    // A declared raw octet body never reaches the JSON pipeline: no parse, no
    // schema, no base64. Its two declared facts are the whole contract, and
    // both are enforced before the handler sees a single octet.
    if (declared.streamed) {
      if (!isConcreteBinaryContentType(ctx.headers.get('Content-Type'))) {
        void ctx.req.body?.cancel().catch(() => {});
        throw new UnsupportedMediaTypeException('A concrete request content type is required');
      }
      const announced = Number.parseInt(ctx.headers.get('Content-Length') ?? '', 10);
      if (!Number.isNaN(announced) && announced > declared.maxBytes) {
        void ctx.req.body?.cancel().catch(() => {});
        throw new PayloadTooLargeException('Request body exceeds the declared bound');
      }
      const body =
        ctx.req.body ??
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.close();
          },
        });
      const bounded = limitBinaryStream(body, declared.maxBytes);
      ctx.body = (async () => bounded) as HttpRequestContext['body'];
    } else {
      ctx.body = (async () => readDeclaredBinaryBody(ctx, declared)) as HttpRequestContext['body'];
    }
  } else if (schemas?.body) {
    const bodySchema = schemas.body;
    const coerceBody = schemas.bodyContentType === 'application/x-www-form-urlencoded';
    const aotBody = aot?.body;
    const originalBody = ctx.body;
    ctx.body = (async () => {
      const raw = await originalBody();
      return runValidator(bodySchema, raw ?? {}, { coerce: coerceBody, label: 'body' }, aotBody);
    }) as HttpRequestContext['body'];
  }

  if (schemas?.headers) {
    // Extract the declared header values from the raw `Headers` object (HTTP
    // header lookup is case-insensitive) and validate them like query/params.
    // The validated record is published on `ctx.headerParams()`; the raw
    // `ctx.headers` object is deliberately left untouched so downstream readers
    // (content negotiation, middleware) keep the native `Headers` API.
    const raw: Record<string, string> = {};
    for (const key of Object.keys(schemas.headers)) {
      const value = ctx.headers.get(key);
      if (value !== null) {
        raw[key] = value;
      }
    }
    const validatedHeaders = runValidator(schemas.headers, raw, { coerce: true, label: 'headers' });
    (ctx as unknown as { headerParams: () => Record<string, unknown> }).headerParams = () => validatedHeaders;
  }
}

function limitBinaryStream(source: ReadableStream<Uint8Array>, maxBytes: number): ReadableStream<Uint8Array> {
  const reader = source.getReader();
  let total = 0;
  let finished = false;
  return new ReadableStream<Uint8Array>(
    {
      async pull(controller) {
        try {
          const item = await reader.read();
          if (finished) return;
          if (item.done) {
            finished = true;
            controller.close();
            return;
          }
          total += item.value.byteLength;
          if (total > maxBytes) {
            finished = true;
            const error = new PayloadTooLargeException('Request body exceeds the declared bound');
            await reader.cancel(error).catch(() => {});
            controller.error(error);
            return;
          }
          controller.enqueue(item.value);
        } catch (error) {
          finished = true;
          controller.error(error);
        }
      },
      async cancel(reason) {
        finished = true;
        await reader.cancel(reason);
      },
    },
    { highWaterMark: 0 },
  );
}

/**
 * Apply a raw octet declaration to the incoming request.
 *
 * The bound is checked twice on purpose. `Content-Length` is advisory, so a
 * peer that announces a small body and sends a large one must still be
 * stopped; and a peer that announces an oversized body must be refused before
 * a single octet of it is read. Only the bounded read is authoritative.
 */
async function readDeclaredBinaryBody(ctx: HttpRequestContext, declared: BinaryMeta): Promise<BinaryBody> {
  // Both refusals are thrown as the framework exception whose name resolves to
  // the stable wire code the contract declares, so a generated client reads
  // `http.unsupported_media_type` / `http.payload_too_large` and not an
  // unattributed remote failure.
  if (baseMediaType(ctx.headers.get('Content-Type')) !== declared.mediaType.toLowerCase()) {
    throw new UnsupportedMediaTypeException('Unsupported request content type');
  }
  const announced = Number.parseInt(ctx.headers.get('Content-Length') ?? '', 10);
  if (!Number.isNaN(announced) && announced > declared.maxBytes) {
    throw new PayloadTooLargeException('Request body exceeds the declared bound');
  }
  const stream = ctx.req.body;
  if (!stream) {
    const buffered = new Uint8Array(await ctx.req.arrayBuffer());
    if (buffered.byteLength > declared.maxBytes)
      throw new PayloadTooLargeException('Request body exceeds the declared bound');
    return buffered;
  }
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    // Read one octet past the bound: `total > maxBytes` then means the payload
    // overflowed rather than merely reached the bound, and the rest of an
    // oversized source is never pulled.
    while (total <= declared.maxBytes) {
      // biome-ignore lint/performance/noAwaitInLoops: body chunks arrive sequentially under the declared bound
      const { done, value } = await reader.read();
      if (done) break;
      if (value && value.byteLength > 0) {
        chunks.push(value);
        total += value.byteLength;
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
  if (total > declared.maxBytes) throw new PayloadTooLargeException('Request body exceeds the declared bound');
  const body: BinaryBody = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}
