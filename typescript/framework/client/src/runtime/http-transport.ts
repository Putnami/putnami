import { type ClientSchema, isConcreteBinaryContentType } from '@putnami/application';
import {
  ClientRequestError,
  ClientRequestEncodingError,
  ClientResponseContractError,
  ClientServerError,
  decodeFrameworkError,
  readFirstPartyErrorEnvelope,
} from './errors';
import { decodeJsonBody, encodeJsonBody, parseJsonValue } from './json-codec';
import { readBodyBytesCapped, resolveMaxResponseSize } from './response-cap';
import { parseRetryAfter } from './retry';
import type { ClientRequest, ClientResponse, Transport } from './transport.type';
import { assertHttpUrl, buildRequestUrl } from './url';
import { markTransportFailure } from './transport-failure';

/** Maximum size (bytes) of a success body parsed without a JSON content-type. */
export const MAX_SPECULATIVE_JSON_BYTES = 1024 * 1024; // 1 MiB

/**
 * HTTP/JSON transport. Generated first-party calls carry their exact request
 * and response schemas, so their bodies use the strict schema-directed codec.
 * The legacy low-level path retains ordinary JSON serialization.
 */
export class HttpTransport implements Transport {
  /** The declared JSON body is decoded from the bytes read, and those bytes travel with the response. */
  readonly carriesSuccessBody = true;
  private readonly baseUrl: string;
  private readonly serviceName: string;
  /** Maximum response body size in bytes (Go parity: 0/unset ⇒ 32 MiB default). */
  private readonly maxResponseSize: number;

  constructor(baseUrl: string, serviceName = '', maxResponseSize?: number) {
    // Reject non-http(s) schemes before the URL ever reaches fetch (SSRF guard).
    const validated = assertHttpUrl(baseUrl, 'HttpTransport baseUrl');
    // Strip trailing slash
    this.baseUrl = validated.endsWith('/') ? validated.slice(0, -1) : validated;
    this.serviceName = serviceName;
    this.maxResponseSize = resolveMaxResponseSize(maxResponseSize);
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: transport response branches preserve typed failures
  async execute<T>(request: ClientRequest): Promise<ClientResponse<T>> {
    const url = buildRequestUrl(this.baseUrl, request);

    const init: RequestInit = {
      method: request.method,
      headers: request.headers,
      signal: request.signal,
    };
    if (request.streamedRequest && !isConcreteBinaryContentType(request.requestMediaType)) {
      if (request.body instanceof ReadableStream) await request.body.cancel().catch(() => {});
      throw new ClientRequestEncodingError('a streamed request requires a concrete content type');
    }
    if (request.streamedRequest) {
      if (!Number.isSafeInteger(request.maxRequestBytes) || (request.maxRequestBytes ?? 0) <= 0) {
        if (request.body instanceof ReadableStream) await request.body.cancel().catch(() => {});
        throw new ClientRequestEncodingError('a streamed request requires a positive byte bound');
      }
      if (
        request.body !== undefined &&
        !(request.body instanceof Uint8Array) &&
        !(request.body instanceof ArrayBuffer) &&
        !(request.body instanceof ReadableStream)
      ) {
        throw new ClientRequestEncodingError('a streamed request requires an octet source');
      }
      const maxBytes = request.maxRequestBytes!;
      if (request.body instanceof Uint8Array && request.body.byteLength > maxBytes) {
        throw new ClientRequestEncodingError('service request body exceeds the declared bound');
      }
      if (request.body instanceof ArrayBuffer && request.body.byteLength > maxBytes) {
        throw new ClientRequestEncodingError('service request body exceeds the declared bound');
      }
      if (request.body instanceof ReadableStream) {
        request.body = boundReadableStream(
          request.body,
          maxBytes,
          () => new ClientRequestEncodingError('service request body exceeds the declared bound'),
        );
      }
      request.headers.set('Content-Type', request.requestMediaType!);
    }

    if (request.body !== undefined && request.method !== 'GET') {
      if (request.requestMediaType) {
        // The payload is already octets. Encoding it as JSON — or as base64
        // inside JSON — is exactly the silent re-wrapping a raw octet
        // declaration exists to refuse.
        request.headers.set('Content-Type', request.requestMediaType);
        init.body = request.body as BodyInit;
        if (request.body instanceof ReadableStream) Object.assign(init, { duplex: 'half' });
      } else {
        request.headers.set('Content-Type', 'application/json');
        init.body = request.requestSchema
          ? encodeJsonBody(request.body, request.requestSchema, request.clientSchemas)
          : JSON.stringify(request.body);
      }
    }

    init.redirect = 'error';
    const response = await fetch(url, init).catch((error: unknown) => {
      throw markTransportFailure(error);
    });

    let data: T | undefined;
    const contentType = response.headers.get('Content-Type') ?? '';
    if (request.streamedResponse && response.status < 400) {
      const declared = request.successes?.find((entry) => entry.status === response.status);
      const content = declared?.content[0];
      if (
        declared?.content.length !== 1 ||
        !content?.streamed ||
        content.mediaType !== '*/*' ||
        !Number.isSafeInteger(content.maxBytes) ||
        (content.maxBytes ?? 0) <= 0 ||
        !isBinaryContent(content) ||
        !isConcreteBinaryContentType(contentType)
      ) {
        await response.body?.cancel().catch(() => {});
        throw new ClientResponseContractError('provider returned an undeclared raw HTTP stream');
      }
      const body =
        response.body ??
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.close();
          },
        });
      return {
        data: (request.maxPayloadBytes
          ? boundReadableStream(
              body,
              request.maxPayloadBytes,
              () =>
                new ClientResponseContractError(
                  'service response exceeds the declared bound',
                  this.serviceName,
                  `${request.method} ${request.path}`,
                ),
            )
          : body) as T,
        status: response.status,
        headers: response.headers,
      };
    }
    // Cap the single body read (Go parity): oversized responses fail rather than
    // buffer unbounded. This is the one read for both success and error bodies.
    // The bytes are read once; the JSON paths decode them, the raw octet path
    // hands them over unchanged.
    const bytes = await readBodyBytesCapped(response, resolveReadCap(request, response.status, this.maxResponseSize), {
      service: this.serviceName,
      method: `${request.method} ${request.path}`,
    });
    const text = new TextDecoder().decode(bytes);
    const success = request.successes?.find((entry) => entry.status === response.status);
    let successBody: Uint8Array | undefined;
    if (request.successes && response.status >= 200 && response.status < 400) {
      if (!success) throw new ClientResponseContractError('provider returned an undeclared success status');
      const decoded = decodeDeclaredSuccess<T>(bytes, text, contentType, success, request);
      data = decoded.data;
      successBody = decoded.successBody;
    } else if (contentType.includes('application/json')) {
      try {
        data =
          text.length > 0
            ? request.clientOperation
              ? (parseJsonValue(text) as T)
              : (JSON.parse(text) as T)
            : undefined;
      } catch {
        if (request.clientOperation) throw new ClientResponseContractError('provider returned malformed JSON');
        throw new ClientResponseContractError('response body is not valid JSON');
      }
    } else {
      // No JSON content-type: only *speculatively* parse small bodies as JSON.
      // A misbehaving/compromised upstream can return a huge non-JSON body, so
      // cap the speculative parse — larger bodies are returned as raw text
      // instead of being fully JSON-parsed and cast to T.
      if (text.length > 0) {
        if (text.length <= MAX_SPECULATIVE_JSON_BYTES) {
          try {
            data = JSON.parse(text) as T;
          } catch {
            data = text as unknown as T;
          }
        } else {
          data = text as unknown as T;
        }
      }
    }

    if (response.status >= 400) {
      // Record the provider's own backoff before any error is raised: the retry
      // interceptor sits outside this transport and never sees these headers.
      if (request.retryState) {
        const retryAfterMs = parseRetryAfter(response.headers.get('retry-after'));
        if (retryAfterMs !== undefined) request.retryState.retryAfterMs = retryAfterMs;
      }
      if (request.clientOperation) {
        // A first-party endpoint always answers with the `{code, error, message,
        // details?}` envelope, so the declared schema is checked against
        // `details` and the stable code is read from `code`.
        const envelope = readFirstPartyErrorEnvelope(data);
        throw decodeFrameworkError({
          service: this.serviceName,
          method: request.operationId ?? `${request.method} ${request.path}`,
          status: response.status,
          payload: data,
          ...(envelope.remoteCode !== undefined ? { remoteCode: envelope.remoteCode } : {}),
          detailsPayload: envelope.detailsPayload,
          operation: request.clientOperation,
          schemas: request.clientSchemas,
          secrets: request.secretValues,
          carryRemoteMessage: request.carryRemoteMessage,
        });
      }
      const ErrorClass = response.status >= 500 ? ClientServerError : ClientRequestError;
      throw new ErrorClass({
        service: this.serviceName,
        method: `${request.method} ${request.path}`,
        status: response.status,
        message: `HTTP ${response.status}: ${response.statusText}`,
        responseBody: data,
      });
    }

    return {
      data: data as T,
      status: response.status,
      headers: response.headers,
      ...(successBody ? { successBody } : {}),
    };
  }
}

/**
 * Resolve the single read cap. The resilience cap bounds everything; a
 * declared payload bound narrows it further, and only on a success — an error
 * envelope is not the payload the contract bounded, and capping it there would
 * make a provider's own refusal unreadable.
 */
function resolveReadCap(request: ClientRequest, status: number, fallback: number): number {
  const cap = request.maxResponseBytes ?? fallback;
  if (request.maxPayloadBytes && status >= 200 && status < 300 && request.maxPayloadBytes < cap) {
    return request.maxPayloadBytes;
  }
  return cap;
}

/** A raw octet representation is exactly `{ type: "string", format: "binary" }`. */
function isBinaryContent(content: { schema?: ClientSchema }): boolean {
  return content.schema?.type === 'string' && content.schema.format === 'binary';
}

function boundReadableStream(
  source: ReadableStream<Uint8Array>,
  maxBytes: number,
  overflow: () => Error,
): ReadableStream<Uint8Array> {
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
            const error = overflow();
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
 * Decode a declared success. `successBody` is the JSON body exactly as the
 * provider sent it, present only when `data` was decoded from a declared JSON
 * document — including the empty body of a status that declares none, which is
 * an accepted answer with no bytes.
 */
function decodeDeclaredSuccess<T>(
  bytes: Uint8Array,
  text: string,
  contentType: string,
  success: NonNullable<ClientRequest['successes']>[number],
  request: ClientRequest,
): { data: T | undefined; successBody?: Uint8Array } {
  if (success.content.length === 0) {
    if (bytes.length > 0)
      throw new ClientResponseContractError('provider returned a body for a declared empty response');
    return { data: undefined, successBody: bytes };
  }
  const mediaType = contentType.split(';', 1)[0]?.trim().toLowerCase();
  const content = success.content.find((entry) => entry.mediaType.toLowerCase() === mediaType);
  if (!content) throw new ClientResponseContractError('provider returned an undeclared response content type');
  if (isBinaryContent(content)) {
    // Zero octets are octets: an empty declared payload is a payload, not a
    // missing one, so the emptiness rule below never applies to it.
    if (content.maxBytes !== undefined && bytes.length > content.maxBytes)
      throw new ClientResponseContractError('provider returned a payload larger than the declared bound');
    return { data: bytes as T };
  }
  if (!content.schema) {
    if (bytes.length > 0) throw new ClientResponseContractError('provider returned an untyped response body');
    return { data: undefined, successBody: bytes };
  }
  if (bytes.length === 0) throw new ClientResponseContractError('provider returned an empty typed response body');
  if (content.mediaType !== 'application/json') {
    throw new ClientResponseContractError('generated REST response codec does not support the declared media type');
  }
  return { data: decodeJsonBody(text, content.schema, request.clientSchemas) as T, successBody: bytes };
}
