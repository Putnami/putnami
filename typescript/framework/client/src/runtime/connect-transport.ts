import {
  type ClientProtobufDescriptor,
  type ClientSchema,
  type ProtoEnumValues,
  type ProtoFieldMeta,
  CONNECT_ACCEPT_ENCODING_HEADER,
  CONNECT_CONTENT_ENCODING_HEADER,
  CONNECT_PROTOCOL_VERSION,
  CONNECT_PROTOCOL_VERSION_HEADER,
  CONNECT_TIMEOUT_HEADER,
  CT_CONNECT_STREAM_JSON,
  CT_CONNECT_STREAM_PROTO,
  decodeProto,
  encodeProto,
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_RESERVED_FLAGS,
} from '@putnami/application';
import { ConnectFrameDecoder, decodeContentEncoding, gunzip, gzip, StreamRelay, writeEnvelope } from './connect-stream';
import {
  ClientRequestError,
  ClientResponseContractError,
  ClientServerError,
  ClientStreamError,
  decodeFrameworkError,
} from './errors';
import {
  type ConnectErrorInfo,
  formatConnectTimeoutMs,
  GrpcStatus,
  parseConnectError,
  parseEndStreamTerminal,
} from './grpc-status';
import { MAX_SPECULATIVE_JSON_BYTES } from './http-transport';
import { decodeJsonBody, decodeJsonValue, encodeJsonBody } from './json-codec';
import { readBodyBytesCapped, resolveMaxResponseSize } from './response-cap';
import { parseRetryAfter } from './retry';
import { validateResponseShape } from './response-validator';
import type { StreamSession } from './stream-session';
import type { StreamObserver } from './stream.type';
import type { ClientRequest, ClientResponse, Transport } from './transport.type';
import { assertHttpUrl } from './url';
import { markTransportFailure } from './transport-failure';

/**
 * Connect protocol encoding mode.
 */
export type ConnectEncoding = 'json' | 'proto';

/** Unary media type for the JSON codec. */
const CT_UNARY_JSON = 'application/json';

/** Unary media type for the binary protobuf codec. */
const CT_UNARY_PROTO = 'application/proto';

/** Per-message encodings this runtime can decode on a stream. */
const STREAM_ACCEPT_ENCODINGS = 'gzip, identity';

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

/**
 * Proto metadata needed for binary encoding/decoding.
 */
export interface ProtoMeta {
  /** Map of message name → field metadata */
  messageMeta: Record<string, ProtoFieldMeta[]>;
  /** Set of enum type names */
  enumTypes?: string[];
  /** Declared members of each enum, in declaration order. */
  enumValues?: ProtoEnumValues;
  /** Fully-qualified method path → its input and output message names. */
  methods?: Record<string, { input: string; output: string; serverStreaming: boolean }>;
}

/**
 * Convert the provider's published descriptor into the codec's metadata.
 *
 * The descriptor is the provider's own projection of its `.proto`, carried in
 * `x-putnami-client.protobuf`, so message names, field numbers, presence and
 * `json_name` all come from the emitter rather than being re-derived here.
 */
export function protoMetaFromDescriptor(descriptor: ClientProtobufDescriptor): ProtoMeta {
  const messageMeta: Record<string, ProtoFieldMeta[]> = {};
  for (const message of descriptor.messages) {
    messageMeta[message.name] = message.fields.map((field) => ({
      name: field.name,
      jsonName: field.jsonName,
      number: field.number,
      type: field.typeKind === 'map' ? 'map' : field.type,
      optional: field.optional === true,
      repeated: field.repeated === true,
      ...(field.oneof ? { oneof: field.oneof } : {}),
      ...(field.map ? { mapKeyType: field.map.keyType, mapValueType: field.map.valueType } : {}),
    }));
  }
  // Enums stay numeric through the codec. The descriptor names each member in
  // protobuf's own SCREAMING_SNAKE form, which is not the string the contract
  // declares, so turning a number back into a declared member is the schema's
  // job — see `projectProtoEnums`.
  const methods: Record<string, { input: string; output: string; serverStreaming: boolean }> = {};
  for (const service of descriptor.services) {
    for (const method of service.methods) {
      methods[`/${descriptor.package}.${service.name}/${method.name}`] = {
        input: method.input,
        output: method.output,
        serverStreaming: method.serverStreaming,
      };
    }
  }
  return { messageMeta, enumTypes: descriptor.enums.map((entry) => entry.name), methods };
}

/**
 * Optional {@link ConnectTransport} behaviour that is not part of the encoding
 * choice.
 */
export interface ConnectTransportOptions {
  /**
   * Per-call timeout in ms, sent as `Connect-Timeout-Ms` so the server enforces
   * the same deadline the client already enforces locally. Streams are
   * long-lived and never carry a derived deadline.
   */
  timeoutMs?: number;
  /**
   * Gzip request bodies and declare `Content-Encoding: gzip`. Off by default:
   * compressing tiny RPC bodies costs more than it saves, and the server always
   * accepts an uncompressed body.
   */
  compressRequests?: boolean;
}

/** Options for a Connect server-streaming call. */
export interface ConnectStreamOptions {
  /** Initial request message. */
  body?: unknown;
  /** Headers to send; may be a promise (auth interceptors resolve asynchronously). */
  headers?: Headers | Promise<Headers>;
  /** Caller cancel signal, combined with `cancel()` on the returned observer. */
  signal?: AbortSignal;
  /**
   * Invoked instead of the error handler when the server answers
   * `unimplemented` (or has no Connect route) before any message was delivered.
   * Return `true` to claim the failure — the caller is expected to re-route the
   * stream onto a fallback transport. Returning `false` surfaces the typed
   * error normally.
   */
  onUnimplemented?: (error: ClientStreamError) => boolean;
}

/**
 * Connect protocol transport, version 1.
 *
 * Two codecs: `json` (`application/json` unary, `application/connect+json`
 * streaming) and `proto` (`application/proto`, `application/connect+proto`).
 * Binary mode uses the same codec the provider uses, driven by the descriptor
 * the provider published, so the two sides cannot disagree about field numbers
 * or presence.
 *
 * - Unary: `POST /{package}.{Service}/{Method}`, bare message body, deadline in
 *   `Connect-Timeout-Ms`. An error is a non-200 status whose body is a JSON
 *   `Error`; a first-party error additionally carries the D0.1 envelope as a
 *   `putnami.client.v1.FrameworkError` detail, which is what turns it back into
 *   the same typed error a REST call would have raised.
 * - Server streaming: a sequence of envelope frames terminated by exactly one
 *   `EndStreamResponse`.
 *
 * Client- and bidi-streaming cannot ride Connect over HTTP/1.1; the server
 * answers those RPCs with `unimplemented` and {@link ConnectStreamOptions.onUnimplemented}
 * lets the caller route them onto the WebSocket transport.
 */
export class ConnectTransport implements Transport {
  private readonly baseUrl: string;
  private readonly packageName: string;
  private readonly encoding: ConnectEncoding;
  private readonly protoMeta?: ProtoMeta;
  private readonly enumTypesSet?: ReadonlySet<string>;
  /** Maximum response body size in bytes (Go parity: 0/unset ⇒ 32 MiB default). */
  private readonly maxResponseSize: number;
  /** `Connect-Timeout-Ms` value derived from the per-call timeout, when configured. */
  private readonly connectTimeout?: string;
  private readonly compressRequests: boolean;

  constructor(
    baseUrl: string,
    packageName: string,
    encoding?: ConnectEncoding,
    protoMeta?: ProtoMeta,
    maxResponseSize?: number,
    options?: ConnectTransportOptions,
  ) {
    const validated = assertHttpUrl(baseUrl, 'ConnectTransport baseUrl');
    this.baseUrl = validated.endsWith('/') ? validated.slice(0, -1) : validated;
    this.packageName = packageName;
    this.encoding = encoding ?? 'json';
    this.protoMeta = protoMeta;
    this.enumTypesSet = protoMeta?.enumTypes ? new Set(protoMeta.enumTypes) : undefined;
    this.maxResponseSize = resolveMaxResponseSize(maxResponseSize);
    this.connectTimeout = options?.timeoutMs !== undefined ? formatConnectTimeoutMs(options.timeoutMs) : undefined;
    this.compressRequests = options?.compressRequests ?? false;
  }

  /** True when this transport encodes bodies as binary protobuf. */
  private get useBinary(): boolean {
    return this.encoding === 'proto' && this.protoMeta !== undefined;
  }

  /**
   * The JSON codec carries the declared document itself, so its bytes are the
   * provider's. The proto codec rebuilds JSON from the protobuf reply, and
   * nothing it could hand over is.
   */
  get carriesSuccessBody(): boolean {
    return !this.useBinary;
  }

  /** The codec this transport negotiated, for a caller that must name it. */
  get codec(): ConnectEncoding {
    return this.encoding;
  }

  async execute<T>(request: ClientRequest): Promise<ClientResponse<T>> {
    const url = `${this.baseUrl}${request.path}`;

    if (this.useBinary) {
      return this.executeProto<T>(url, request);
    }
    return this.executeJson<T>(url, request);
  }

  private async executeJson<T>(url: string, request: ClientRequest): Promise<ClientResponse<T>> {
    request.headers.set('Content-Type', CT_UNARY_JSON);
    request.headers.set('Accept', CT_UNARY_JSON);
    const message = this.buildRequestMessage(request);
    const bodyBytes = textEncoder.encode(
      request.requestSchema && request.clientOperation === undefined
        ? encodeJsonBody(message, request.requestSchema, request.clientSchemas)
        : JSON.stringify(message),
    );

    const { response, contentType } = await this.doFetch(
      url,
      request,
      await this.encodeRequestBody(bodyBytes, request),
    );
    const bytes = await this.readBody(response, request.path, request.maxResponseBytes);
    if (request.clientOperation) {
      return this.decodeDeclaredJsonSuccess<T>(bytes, contentType, response, request);
    }
    const data = this.parseJsonBody<T>(bytes, contentType);
    this.validateResponse(data, request.path);
    return { data, status: response.status, headers: response.headers };
  }

  private async executeProto<T>(url: string, request: ClientRequest): Promise<ClientResponse<T>> {
    const messageNames = this.resolveMessageNames(request.path);

    const bodyBytes = this.encodeProtoMessage(this.buildProtoRequestMessage(request), messageNames.input);

    request.headers.set('Content-Type', CT_UNARY_PROTO);
    request.headers.set('Accept', CT_UNARY_PROTO);

    const { response, contentType } = await this.doFetch(
      url,
      request,
      await this.encodeRequestBody(bodyBytes, request),
    );
    const bytes = await this.readBody(response, request.path, request.maxResponseBytes);

    // Decode binary proto response if available
    const responseFields = this.protoMeta?.messageMeta[messageNames.output];
    if (contentType.includes('proto') && responseFields) {
      const decoded = this.decodeProtoMessage(bytes, responseFields);
      if (request.clientOperation) {
        return {
          data: this.projectDeclaredSuccess<T>(decoded, response.status, request),
          status: response.status,
          headers: response.headers,
        };
      }
      this.validateResponse(decoded, request.path, messageNames.output);
      return { data: decoded as T, status: response.status, headers: response.headers };
    }

    // Fallback to JSON response parsing
    const data = this.parseJsonBody<T>(bytes, contentType);
    this.validateResponse(data, request.path, messageNames.output);
    return { data, status: response.status, headers: response.headers };
  }

  /**
   * Build the Connect request message: the `{params, query, body}` envelope.
   *
   * Every first-party provider, Go or TypeScript, publishes its request message
   * as these three named sections, so a generated method hands its path
   * parameters, query parameters and body over unchanged whichever language
   * answers. A section the call does not carry is omitted. The declared enums of
   * the body are projected to their wire form inside the `body` section.
   */
  private buildRequestMessage(request: ClientRequest): Record<string, unknown> {
    if (!request.clientOperation) {
      return (request.body as Record<string, unknown>) ?? {};
    }
    const message: Record<string, unknown> = {};
    const section = (values: Record<string, unknown> | undefined): Record<string, unknown> | undefined => {
      const defined = Object.entries(values ?? {}).filter(([, value]) => value !== undefined);
      return defined.length > 0 ? Object.fromEntries(defined) : undefined;
    };
    const params = section(request.params);
    if (params) message['params'] = params;
    // A repeated query parameter has no single Connect field; the contract
    // declares it as a list, so it travels as one.
    const query = section(request.query as Record<string, unknown> | undefined);
    if (query) message['query'] = query;
    if (request.body !== undefined && request.body !== null) {
      if (typeof request.body !== 'object' || Array.isArray(request.body)) {
        throw new ClientResponseContractError('Connect carries a request body only as a declared object');
      }
      message['body'] = request.body;
    }
    return message;
  }

  /** The request message with the body's declared enums in their protobuf form. */
  private buildProtoRequestMessage(request: ClientRequest): Record<string, unknown> {
    const message = this.buildRequestMessage(request);
    if (!request.clientOperation) {
      return projectDeclaredEnums(message, request.requestSchema, request.clientSchemas) as Record<string, unknown>;
    }
    if (message['body'] !== undefined) {
      message['body'] = projectDeclaredEnums(message['body'], request.requestSchema, request.clientSchemas);
    }
    return message;
  }

  /** Decode a declared success from a Connect JSON body under its declared schema. */
  private decodeDeclaredJsonSuccess<T>(
    bytes: Uint8Array,
    contentType: string,
    response: Response,
    request: ClientRequest,
  ): ClientResponse<T> {
    const declared = this.declaredSuccessSchema(response.status, request);
    const text = textDecoder.decode(bytes);
    if (!declared) {
      if (text.trim().length > 0 && text.trim() !== '{}') {
        throw new ClientResponseContractError('provider returned a body for a declared empty response');
      }
      return { data: undefined as T, status: response.status, headers: response.headers };
    }
    if (!contentType.includes('json')) {
      throw new ClientResponseContractError('provider returned an undeclared Connect response content type');
    }
    if (text.length === 0) throw new ClientResponseContractError('provider returned an empty typed response body');
    // The Connect JSON codec carries the declared document itself, so the
    // bytes read are the body the schema accepted.
    return {
      data: decodeJsonBody(text, declared, request.clientSchemas) as T,
      status: response.status,
      headers: response.headers,
      successBody: bytes,
    };
  }

  /** Validate a decoded proto message against the operation's declared success schema. */
  private projectDeclaredSuccess<T>(decoded: unknown, status: number, request: ClientRequest): T {
    const declared = this.declaredSuccessSchema(status, request);
    if (!declared) return undefined as T;
    try {
      const projected = projectProtoEnums(decoded, declared, request.clientSchemas);
      return decodeJsonValue(projected, declared, request.clientSchemas) as T;
    } catch {
      throw new ClientResponseContractError('provider response does not match its declared schema');
    }
  }

  /**
   * The schema the operation declared for this status.
   *
   * Connect answers success with HTTP 200 whatever status the REST declaration
   * names, so a single declared success is used as-is; several declared
   * successes have no Connect representation and the generator refuses them
   * before a client is emitted.
   */
  private declaredSuccessSchema(status: number, request: ClientRequest): ClientSchema | undefined {
    const successes = request.successes ?? [];
    const exact = successes.find((entry) => entry.status === status);
    const chosen = exact ?? (successes.length === 1 ? successes[0] : undefined);
    if (!chosen) {
      if (successes.length === 0) return undefined;
      throw new ClientResponseContractError('provider returned an undeclared success status');
    }
    return chosen.content.find((entry) => entry.mediaType === 'application/json')?.schema;
  }

  /**
   * Consume a Connect **server-streaming** RPC.
   *
   * The response is a sequence of envelope frames terminated by exactly one
   * `EndStreamResponse`; a terminal carrying an error is delivered as
   * {@link ClientStreamError} on `onError`. `cancel()` aborts the underlying
   * fetch, which closes the connection and stops the server's writer.
   */
  stream<T>(path: string, options?: ConnectStreamOptions): StreamObserver<T> {
    const relay = new StreamRelay<T>();
    this.streamInto(relay, path, options);
    return relay;
  }

  /**
   * Drive a Connect server-streaming call into an existing {@link StreamRelay}.
   *
   * Kept separate from {@link stream} so a caller that owns the relay (and can
   * therefore re-point it at a fallback transport) reuses the same handlers the
   * caller already registered.
   */
  streamInto<T>(relay: StreamRelay<T>, path: string, options?: ConnectStreamOptions): void {
    const controller = new AbortController();
    relay.attach(() => controller.abort());

    // biome-ignore lint/complexity/noVoid: relay owns the deliberately detached stream lifecycle
    void this.runStream(relay, path, controller, options).catch((error) => {
      // A cancel() aborts the fetch; that rejection is the caller's own doing,
      // not a stream failure to report back to them.
      if (relay.isCancelled) return;
      relay.fail(error instanceof Error ? error : new Error(String(error)));
    });
  }

  /**
   * Drive a first-party Connect server stream under a {@link StreamSession}.
   *
   * The session owns admission, the four budgets, the breaker rule and the
   * single call measurement; this transport only turns bytes into values.
   * Admission is the accepted response headers — a 200 whose content type is the
   * streaming media type — and never the first message.
   */
  openServiceStream<T>(request: ClientRequest, session: StreamSession<T>, options: { output?: ClientSchema }): void {
    // biome-ignore lint/complexity/noVoid: the session owns the detached stream lifecycle
    void this.runServiceStream(request, session, options).catch((error) => {
      session.fail(error);
      session.close();
    });
  }

  private async runServiceStream<T>(
    request: ClientRequest,
    session: StreamSession<T>,
    options: { output?: ClientSchema },
  ): Promise<void> {
    const attempt = session.beginAttempt();
    const headers = new Headers(request.headers);
    const binary = this.useBinary;
    const messageNames = binary ? this.resolveMessageNames(request.path) : undefined;
    const payload =
      binary && messageNames
        ? this.encodeProtoMessage(this.buildProtoRequestMessage(request), messageNames.input)
        : textEncoder.encode(JSON.stringify(this.buildRequestMessage(request)));

    headers.set('Content-Type', binary ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON);
    headers.set('Accept', binary ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON);
    headers.set(CONNECT_PROTOCOL_VERSION_HEADER, CONNECT_PROTOCOL_VERSION);
    headers.set(CONNECT_ACCEPT_ENCODING_HEADER, STREAM_ACCEPT_ENCODINGS);

    session.dispatch();
    const response = await fetch(`${this.baseUrl}${request.path}`, {
      method: 'POST',
      headers,
      signal: attempt.signal,
      // A Connect request stream is enveloped, even when it carries exactly one
      // message: a bare body is a unary call, and the server reads it as one.
      body: toArrayBuffer(writeEnvelope(0x00, payload)),
    });

    if (!response.ok) {
      const info = await this.readErrorInfo(response, request.path);
      attempt.finish({ status: response.status, error: info });
      if (response.status === 401 || response.status === 403) session.invalidateCredentials();
      session.fail(this.streamFailure(request, response.status, info));
      session.close();
      return;
    }

    const contentType = response.headers.get('Content-Type') ?? '';
    const expected = binary ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON;
    if (!contentType.split(';')[0].trim().startsWith('application/connect+')) {
      attempt.finish({ status: response.status });
      session.fail(
        new ClientResponseContractError(
          'provider answered a Connect stream with a non-streaming content type',
          this.packageName,
          request.operationId ?? request.path,
        ),
      );
      session.close();
      return;
    }
    if (contentType.split(';')[0].trim() !== expected) {
      attempt.finish({ status: response.status });
      session.fail(
        new ClientResponseContractError(
          'provider answered a Connect stream with a codec the call did not negotiate',
          this.packageName,
          request.operationId ?? request.path,
        ),
      );
      session.close();
      return;
    }

    // Admission is the accepted headers, not the first message.
    session.admit();
    attempt.finish({ status: response.status });
    await this.pumpServiceFrames(request, session, response, binary, options.output);
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one loop enforces the frame bound, the compression rule, the reserved bits and the single-terminal rule
  private async pumpServiceFrames<T>(
    request: ClientRequest,
    session: StreamSession<T>,
    response: Response,
    binary: boolean,
    output?: ClientSchema,
  ): Promise<void> {
    const context = { service: this.packageName, method: request.operationId ?? request.path };
    const frameBound = request.maxResponseBytes ?? this.maxResponseSize;
    const decoder = new ConnectFrameDecoder(frameBound, context);
    const streamEncoding = response.headers.get(CONNECT_CONTENT_ENCODING_HEADER);
    const messageNames = binary ? this.resolveMessageNames(request.path) : undefined;
    const responseFields = messageNames ? this.protoMeta?.messageMeta[messageNames.output] : undefined;

    const body = response.body;
    if (!body) {
      session.fail(this.missingTerminal(request));
      session.close();
      return;
    }

    const reader = body.getReader();
    let terminated = false;
    try {
      read: while (true) {
        // biome-ignore lint/performance/noAwaitInLoops: network frames must be read in wire order
        const { done, value } = await reader.read();
        if (done) break;
        for (const frame of decoder.push(value ?? new Uint8Array(0))) {
          if (terminated) {
            // "must not send an EndStreamResponse earlier in the stream" — a
            // frame after the terminal means the peer sent two terminals or
            // kept writing past one.
            session.fail(this.protocolFailure(request, 'provider wrote a frame after the end-of-stream message'));
            break read;
          }
          if ((frame.flags & ENVELOPE_RESERVED_FLAGS) !== 0) {
            session.fail(this.protocolFailure(request, 'provider set a reserved Connect envelope flag'));
            break read;
          }
          let payload = frame.payload;
          if ((frame.flags & ENVELOPE_FLAG_COMPRESSED) !== 0) {
            if (streamEncoding !== 'gzip') {
              session.fail(
                this.protocolFailure(request, 'provider compressed a frame without declaring a stream encoding'),
              );
              break read;
            }
            // biome-ignore lint/performance/noAwaitInLoops: each compressed envelope must decode before delivery
            payload = await gunzip(frame.payload, frameBound, context);
          }

          if (frame.endStream) {
            terminated = true;
            this.applyTerminal(request, session, payload);
            continue;
          }
          if (session.isCancelled) break read;
          session.deliver(this.decodeStreamMessage<T>(payload, binary, responseFields, output, request));
        }
      }
    } finally {
      await reader.cancel().catch(() => {});
    }

    if (!terminated && !session.isCancelled) session.fail(this.missingTerminal(request));
    session.close();
  }

  /** Apply the single `EndStreamResponse` the stream ends with. */
  private applyTerminal<T>(request: ClientRequest, session: StreamSession<T>, payload: Uint8Array): void {
    const terminal = parseEndStreamTerminal(safeJsonParse(textDecoder.decode(payload)));
    if (!terminal.valid) {
      session.fail(this.protocolFailure(request, 'provider wrote a malformed end-of-stream message'));
      return;
    }
    if (!terminal.failure) {
      session.complete();
      return;
    }
    session.fail(this.streamFailure(request, 200, terminal.failure));
  }

  /** Decode one stream payload under the declared message schema. */
  private decodeStreamMessage<T>(
    payload: Uint8Array,
    binary: boolean,
    responseFields: ProtoFieldMeta[] | undefined,
    output: ClientSchema | undefined,
    request: ClientRequest,
  ): T {
    const value =
      binary && responseFields
        ? this.decodeProtoMessage(payload, responseFields)
        : safeJsonParse(textDecoder.decode(payload));
    if (!output) return value as T;
    try {
      return decodeJsonValue(value, output, request.clientSchemas) as T;
    } catch {
      throw new ClientResponseContractError(
        'provider stream message does not match its declared schema',
        this.packageName,
        request.operationId ?? request.path,
      );
    }
  }

  /** Turn a Connect failure into the typed error the operation declared, when it declared one. */
  private streamFailure(request: ClientRequest, status: number, info: ConnectErrorInfo): Error {
    const typed = this.typedFrameworkError(request, info);
    if (typed) return typed;
    return new ClientStreamError({
      service: this.packageName,
      method: request.operationId ?? request.path,
      message: info.message ?? `Connect stream failed with ${info.status}`,
      grpcCode: info.code,
      grpcStatus: info.status,
      ...(info.details ? { details: info.details } : {}),
      // A stream refused before its first message failed at the HTTP layer, and
      // that status is what tells an unauthenticated call from a forbidden one.
      ...(status && status !== 200 ? { status } : {}),
    });
  }

  private protocolFailure(request: ClientRequest, detail: string): Error {
    return new ClientResponseContractError(detail, this.packageName, request.operationId ?? request.path);
  }

  private missingTerminal(request: ClientRequest): Error {
    return this.protocolFailure(request, 'Connect stream ended without an end-of-stream message');
  }

  /**
   * Rebuild the D0.1 typed error from the framework detail the provider attached.
   *
   * The Connect code is a category; the stable framework code and the declared
   * `details` member travel in the `putnami.client.v1.FrameworkError` detail, so
   * a declared error narrows to the same generated type it would have over REST.
   */
  private typedFrameworkError(request: ClientRequest, info: ConnectErrorInfo): Error | undefined {
    if (!request.clientOperation || !info.framework) return undefined;
    return decodeFrameworkError({
      service: this.packageName,
      method: request.operationId ?? request.path,
      status: info.framework.status,
      payload: undefined,
      remoteCode: info.framework.code,
      detailsPayload: info.framework.details,
      operation: request.clientOperation,
      schemas: request.clientSchemas,
      secrets: request.secretValues,
      carryRemoteMessage: request.carryRemoteMessage,
    });
  }

  /** Read envelope frames off the response stream until the trailer frame arrives. */
  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: sequential frame decoding keeps envelope state explicit
  private async pumpFrames<T>(
    relay: StreamRelay<T>,
    path: string,
    response: Response,
    contentType: string,
  ): Promise<void> {
    const context = { service: this.packageName, method: path };
    const decoder = new ConnectFrameDecoder(this.maxResponseSize, context);
    const binaryPayloads = contentType.includes('proto');
    const responseFields = binaryPayloads
      ? this.protoMeta?.messageMeta[this.resolveMessageNames(path).output]
      : undefined;
    const streamEncoding = response.headers.get(CONNECT_CONTENT_ENCODING_HEADER);

    const body = response.body;
    if (!body) {
      relay.fail(this.missingTerminalRelay(path));
      return;
    }

    const reader = body.getReader();
    let terminal: ReturnType<typeof parseEndStreamTerminal> | undefined;
    let violation: string | undefined;
    try {
      read: while (true) {
        // biome-ignore lint/performance/noAwaitInLoops: network frames must be read in wire order
        const { done, value } = await reader.read();
        if (done) break;
        for (const frame of decoder.push(value ?? new Uint8Array(0))) {
          if (terminal) {
            violation = 'provider wrote a frame after the end-of-stream message';
            break read;
          }
          if ((frame.flags & ENVELOPE_RESERVED_FLAGS) !== 0) {
            violation = 'provider set a reserved Connect envelope flag';
            break read;
          }
          let payload = frame.payload;
          if ((frame.flags & ENVELOPE_FLAG_COMPRESSED) !== 0) {
            if (streamEncoding !== 'gzip') {
              violation = 'provider compressed a frame without declaring a stream encoding';
              break read;
            }
            // biome-ignore lint/performance/noAwaitInLoops: each compressed envelope must decode before delivery
            payload = await gunzip(frame.payload, this.maxResponseSize, context);
          }

          if (frame.endStream) {
            terminal = parseEndStreamTerminal(safeJsonParse(textDecoder.decode(payload)));
            continue;
          }
          if (relay.isCancelled) break read;
          relay.handlers.message?.(
            (binaryPayloads && responseFields
              ? this.decodeProtoMessage(payload, responseFields)
              : safeJsonParse(textDecoder.decode(payload))) as T,
          );
        }
      }
    } finally {
      await reader.cancel().catch(() => {});
    }

    if (relay.isCancelled) return;

    if (violation) {
      relay.fail(new ClientResponseContractError(violation, this.packageName, path));
      return;
    }

    if (!terminal?.valid) {
      relay.fail(this.missingTerminalRelay(path));
      return;
    }

    if (terminal.failure) {
      relay.fail(
        new ClientStreamError({
          service: this.packageName,
          method: path,
          message: terminal.failure.message ?? `Stream failed with ${terminal.failure.status}`,
          grpcCode: terminal.failure.code,
          grpcStatus: terminal.failure.status,
          ...(terminal.failure.details ? { details: terminal.failure.details } : {}),
        }),
      );
      return;
    }

    relay.handlers.complete?.();
  }

  private missingTerminalRelay(path: string): ClientStreamError {
    // The connection ended without a readable end-of-stream message: the caller
    // must not read a truncated stream as a successful completion.
    return new ClientStreamError({
      service: this.packageName,
      method: path,
      message: 'Connect stream ended without a valid end-of-stream message',
      grpcCode: GrpcStatus.INTERNAL,
      grpcStatus: 'INTERNAL',
    });
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: protocol negotiation failures retain local fallbacks
  private async runStream<T>(
    relay: StreamRelay<T>,
    path: string,
    controller: AbortController,
    options?: ConnectStreamOptions,
  ): Promise<void> {
    const headers = new Headers(options?.headers ? await options.headers : undefined);
    if (relay.isCancelled) return;

    const binary = this.useBinary;
    const bodyBytes = binary
      ? this.encodeProtoMessage(options?.body, this.resolveMessageNames(path).input)
      : textEncoder.encode(options?.body !== undefined ? JSON.stringify(options.body) : '{}');

    const mediaType = binary ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON;
    headers.set('Content-Type', mediaType);
    headers.set('Accept', mediaType);
    this.applyProtocolHeaders(headers, { deadline: false });
    headers.set(CONNECT_ACCEPT_ENCODING_HEADER, STREAM_ACCEPT_ENCODINGS);

    const signal = options?.signal ? AbortSignal.any([options.signal, controller.signal]) : controller.signal;
    const response = await fetch(`${this.baseUrl}${path}`, {
      method: 'POST',
      headers,
      signal,
      body: toArrayBuffer(writeEnvelope(0x00, bodyBytes)),
    });

    if (!response.ok) {
      const info = await this.readErrorInfo(response, path);
      const error = new ClientStreamError({
        service: this.packageName,
        method: path,
        message: info.message ?? `HTTP ${response.status}`,
        grpcCode: info.code,
        grpcStatus: info.status,
        ...(info.details ? { details: info.details } : {}),
      });
      // `unimplemented` is the server naming a mode Connect cannot carry over
      // HTTP/1.1 (client/bidi streaming). Hand it to the fallback rather than
      // surfacing a raw 501 to the caller.
      if (info.code === GrpcStatus.UNIMPLEMENTED && options?.onUnimplemented?.(error)) return;
      relay.fail(error);
      return;
    }

    const contentType = response.headers.get('Content-Type') ?? '';
    if (!contentType.split(';')[0].trim().startsWith('application/connect+')) {
      // A Connect route that answered unary for a streaming RPC is not a stream
      // we can consume; treat it like `unimplemented` so the fallback engages.
      const error = new ClientStreamError({
        service: this.packageName,
        method: path,
        message: `Connect streaming not supported: server answered '${contentType || 'unknown content type'}'`,
        grpcCode: GrpcStatus.UNIMPLEMENTED,
        grpcStatus: 'UNIMPLEMENTED',
      });
      if (options?.onUnimplemented?.(error)) return;
      relay.fail(error);
      return;
    }

    await this.pumpFrames(relay, path, response, contentType);
  }

  /** Encode a request message to bytes using the embedded proto metadata. */
  private encodeProtoMessage(body: unknown, requestMessageName: string): Uint8Array {
    const requestFields = this.protoMeta?.messageMeta[requestMessageName];
    if (!body || !requestFields) return new Uint8Array(0);
    return encodeProto(
      body as Record<string, unknown>,
      requestFields,
      this.protoMeta?.messageMeta,
      this.enumTypesSet,
      this.protoMeta?.enumValues,
    );
  }

  private decodeProtoMessage(bytes: Uint8Array, fields: ProtoFieldMeta[]): Record<string, unknown> {
    return decodeProto(bytes, fields, this.protoMeta?.messageMeta, this.enumTypesSet, this.protoMeta?.enumValues);
  }

  /**
   * Resolve the request and response message names for an RPC path.
   *
   * The descriptor names them; falling back to `{Method}Request`/`{Method}Response`
   * keeps a hand-configured transport working, and is the convention the emitter
   * follows anyway.
   */
  private resolveMessageNames(path: string): { input: string; output: string } {
    const declared = this.protoMeta?.methods?.[path];
    if (declared) return { input: declared.input, output: declared.output };
    const method = extractRpcMethod(path);
    return { input: `${method}Request`, output: `${method}Response` };
  }

  /**
   * Runtime-validate a parsed/decoded response body against the embedded proto
   * field metadata for the method's response message, before it is returned as
   * the method's declared type.
   *
   * Safe-by-default but non-breaking: when no metadata is embedded for this
   * method's response message (e.g. an unmodeled endpoint), the body is returned
   * unchanged rather than throwing.
   *
   * @throws ClientResponseValidationError when the body violates the shape.
   */
  private validateResponse(data: unknown, requestPath: string, responseMessageName?: string): void {
    const messageMeta = this.protoMeta?.messageMeta;
    if (!messageMeta) return;
    const messageName = responseMessageName ?? this.resolveMessageNames(requestPath).output;
    const responseFields = messageMeta[messageName];
    // No embedded shape for this response — fall back to current behavior.
    if (!responseFields) return;
    validateResponseShape(data, responseFields, messageMeta, this.packageName, requestPath);
  }

  /**
   * Add the Connect protocol headers every unary call carries: the protocol
   * version, the encodings we can decode, and the deadline derived from the
   * per-call timeout. Headers an interceptor already set win.
   */
  private applyProtocolHeaders(headers: Headers, options?: { deadline?: boolean }): void {
    if (!headers.has(CONNECT_PROTOCOL_VERSION_HEADER)) {
      headers.set(CONNECT_PROTOCOL_VERSION_HEADER, CONNECT_PROTOCOL_VERSION);
    }
    if (!headers.has('accept-encoding')) headers.set('accept-encoding', STREAM_ACCEPT_ENCODINGS);
    if (options?.deadline !== false && this.connectTimeout && !headers.has(CONNECT_TIMEOUT_HEADER)) {
      headers.set(CONNECT_TIMEOUT_HEADER, this.connectTimeout);
    }
  }

  /** Apply protocol headers and optional request compression to a unary body. */
  private async encodeRequestBody(bodyBytes: Uint8Array, request: ClientRequest): Promise<BodyInit> {
    this.applyProtocolHeaders(request.headers);
    this.applyRequestDeadline(request);
    return this.compressBody(bodyBytes, request.headers);
  }

  /**
   * Carry the call's own remaining budget as the server-side deadline.
   *
   * The retry interceptor sets `deadlineAt` per attempt; sending the client's
   * configured timeout instead would let the server keep working after the
   * caller has already given up.
   */
  private applyRequestDeadline(request: ClientRequest): void {
    if (request.deadlineAt === undefined) return;
    const remaining = formatConnectTimeoutMs(request.deadlineAt - Date.now());
    if (remaining) request.headers.set(CONNECT_TIMEOUT_HEADER, remaining);
  }

  /**
   * Gzip the request body when request compression is enabled, declaring it via
   * `Content-Encoding` — the header a unary Connect request uses.
   */
  private async compressBody(bodyBytes: Uint8Array, headers: Headers): Promise<BodyInit> {
    const bytes = this.compressRequests ? await gzip(bodyBytes) : bodyBytes;
    if (this.compressRequests) headers.set('Content-Encoding', 'gzip');
    return toArrayBuffer(bytes);
  }

  /**
   * Shared fetch + error handling for both JSON and Proto modes.
   */
  private async doFetch(
    url: string,
    request: ClientRequest,
    body: BodyInit,
  ): Promise<{ response: Response; contentType: string }> {
    const init: RequestInit = {
      method: 'POST',
      headers: request.headers,
      signal: request.signal,
      body,
      redirect: 'error',
    };

    const response = await fetch(url, init).catch((error: unknown) => {
      throw markTransportFailure(error);
    });
    const contentType = response.headers.get('Content-Type') ?? '';

    if (!response.ok) {
      await this.throwConnectError(response, request);
    }

    return { response, contentType };
  }

  /**
   * Read the response body through the size cap (Go parity) and undo any
   * content encoding the fetch implementation left in place.
   */
  private async readBody(response: Response, requestPath?: string, maxBytes?: number): Promise<Uint8Array> {
    const context = { service: this.packageName, method: requestPath };
    const cap = maxBytes ?? this.maxResponseSize;
    const bytes = await readBodyBytesCapped(response, cap, context);
    return decodeContentEncoding(bytes, response.headers, cap, context);
  }

  /**
   * Parse a JSON (or text fallback) response body.
   */
  private parseJsonBody<T>(bytes: Uint8Array, contentType: string): T {
    const text = textDecoder.decode(bytes);
    if (contentType.includes('json')) {
      return (text.length > 0 ? JSON.parse(text) : undefined) as T;
    }
    // Only speculatively parse a non-JSON-typed body up to the same cap as
    // HttpTransport, so a misbehaving upstream returning a huge body cannot force
    // an unbounded JSON.parse.
    if (text.length <= MAX_SPECULATIVE_JSON_BYTES) {
      try {
        return JSON.parse(text) as T;
      } catch {
        // Not JSON — fall through and return the raw text.
      }
    }
    return text as unknown as T;
  }

  /** Read and decode a Connect error body into its status parts. */
  private async readErrorInfo(response: Response, requestPath?: string): Promise<ConnectErrorInfo & { body: unknown }> {
    const contentType = response.headers.get('Content-Type') ?? '';
    let errorBody: unknown;
    try {
      // Cap the error body read too (Go caps the unary body read unconditionally).
      const text = textDecoder.decode(await this.readBody(response, requestPath));
      errorBody = contentType.includes('json') ? (text.length > 0 ? JSON.parse(text) : undefined) : text;
    } catch {
      errorBody = undefined;
    }
    return { ...parseConnectError(errorBody, response.status), body: errorBody };
  }

  private async throwConnectError(response: Response, request: ClientRequest): Promise<never> {
    const info = await this.readErrorInfo(response, request.path);

    // Record the provider's own backoff before any error is raised: the retry
    // interceptor sits outside this transport and never sees these headers.
    if (request.retryState) {
      const retryAfterMs = parseRetryAfter(response.headers.get('retry-after'));
      if (retryAfterMs !== undefined) request.retryState.retryAfterMs = retryAfterMs;
    }

    const typed = this.typedFrameworkError(request, info);
    if (typed) throw typed;

    const ErrorClass = response.status >= 500 ? ClientServerError : ClientRequestError;
    throw new ErrorClass({
      service: this.packageName,
      method: request.operationId ?? request.path,
      status: response.status,
      message: info.message ?? `HTTP ${response.status}`,
      responseBody: info.body,
      grpcCode: info.code,
      grpcStatus: info.status,
      ...(info.details ? { details: info.details } : {}),
    });
  }
}

/**
 * Turn the numbers a protobuf enum carries into the members the contract declares.
 *
 * JSON and protobuf are two encodings of one message: the JSON codec carries
 * `"retired"`, protobuf carries `2`. The declared schema is the only place the
 * member list and its order live, so the substitution is driven from there —
 * position `i` is number `i + 1`, and `0` is the `_UNSPECIFIED` member the
 * declared union has no name for.
 */
function projectProtoEnums(
  value: unknown,
  schema: ClientSchema | undefined,
  schemas?: Readonly<Record<string, ClientSchema>>,
): unknown {
  return walkDeclaredEnums(value, schema, schemas, (members, current) => {
    if (typeof current !== 'number') return current;
    return current === 0 ? undefined : (members[current - 1] ?? current);
  });
}

/** The inverse: a declared member becomes the number protobuf carries. */
function projectDeclaredEnums(
  value: unknown,
  schema: ClientSchema | undefined,
  schemas?: Readonly<Record<string, ClientSchema>>,
): unknown {
  return walkDeclaredEnums(value, schema, schemas, (members, current) => {
    if (typeof current !== 'string') return current;
    const index = members.indexOf(current);
    return index >= 0 ? index + 1 : current;
  });
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: the walk mirrors every branch of the neutral schema union
function walkDeclaredEnums(
  value: unknown,
  schema: ClientSchema | undefined,
  schemas: Readonly<Record<string, ClientSchema>> | undefined,
  convert: (members: readonly string[], current: unknown) => unknown,
): unknown {
  if (!schema || value === undefined || value === null) return value;
  if (schema.$ref) {
    const name = schema.$ref.slice('#/components/schemas/'.length);
    return walkDeclaredEnums(value, schemas?.[name], schemas, convert);
  }
  const members = schema.enum?.every((entry) => typeof entry === 'string')
    ? (schema.enum as readonly string[])
    : undefined;
  if (members) return convert(members, value);
  if (schema.items && Array.isArray(value)) {
    return value.map((entry) => walkDeclaredEnums(entry, schema.items, schemas, convert));
  }
  if (typeof value !== 'object' || Array.isArray(value)) return value;
  const record = value as Record<string, unknown>;
  const properties = schema.properties;
  const additional = typeof schema.additionalProperties === 'object' ? schema.additionalProperties : undefined;
  if (!properties && !additional) return value;
  const projected: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(record)) {
    const childSchema = properties?.[key] ?? additional;
    const next = walkDeclaredEnums(child, childSchema, schemas, convert);
    if (next !== undefined) projected[key] = next;
  }
  return projected;
}

/** Copy bytes into a standalone `ArrayBuffer` suitable for `fetch`'s `body`. */
function toArrayBuffer(bytes: Uint8Array): ArrayBuffer {
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
}

/** Parse JSON, returning the raw text when the payload is not valid JSON. */
function safeJsonParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/**
 * Extract the RPC method name from a Connect path.
 * e.g. "/myapp.v1.UsersService/ListUsers" → "ListUsers"
 */
function extractRpcMethod(path: string): string {
  const lastSlash = path.lastIndexOf('/');
  return lastSlash >= 0 ? path.substring(lastSlash + 1) : path;
}
