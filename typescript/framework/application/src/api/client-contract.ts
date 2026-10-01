/** Version of the first-party generated-client contract carried in OpenAPI. */
export const CLIENT_CONTRACT_PROTOCOL_VERSION = 1 as const;

/** A credential profile names how a consumer resolves a secret without storing its value in generated code. */
export type ClientCredentialProfile =
  | { readonly kind: 'service-token'; readonly audience?: string; readonly scopes?: readonly string[] }
  | { readonly kind: 'forwarded-user-token' }
  | { readonly kind: 'api-key'; readonly header: string }
  | { readonly kind: 'named-header'; readonly header: string };

/** One named credential requirement inside an AND security group. */
export interface ClientSecurityRequirement {
  readonly profile: string;
  readonly scopes?: readonly string[];
  readonly roles?: readonly string[];
}

/** All requirements in an alternative must be satisfied together. */
export interface ClientSecurityAlternative {
  readonly allOf: readonly ClientSecurityRequirement[];
}

/** Alternatives are tried as OR branches. An empty allOf is the explicit anonymous branch. */
export interface ClientSecurityPolicy {
  readonly alternatives: readonly ClientSecurityAlternative[];
  readonly authorization?: ClientAuthorizationPolicy;
}

/** Declarative authorization checks enforced by the provider. */
export interface ClientAuthorizationPolicy {
  readonly issuers?: readonly string[];
  readonly audiences?: readonly string[];
  readonly principalKinds?: readonly string[];
  readonly clients?: readonly string[];
  readonly scopesAll?: readonly string[];
  readonly scopesAny?: readonly string[];
  readonly rolesAll?: readonly string[];
  readonly rolesAny?: readonly string[];
  readonly scopeClaims?: readonly string[];
  readonly roleClaims?: readonly string[];
}

/** The wire transport families a first-party operation can be carried on. */
export type ClientTransportProtocol = 'rest-json' | 'connect' | 'sse' | 'websocket';

/** Ordered wire transport advertised for one operation. */
export interface ClientTransportContract {
  readonly protocol: ClientTransportProtocol;
  readonly path: string;
  /**
   * What one payload is. `binary` is raw octets in binary WebSocket messages,
   * and only a provider-owned wire carries it.
   */
  readonly encoding: 'json' | 'proto' | 'binary';
  readonly protobufMethod?: string;
  readonly websocket?: ClientWebSocketTransport;
  /** Continuation metadata, only on an `sse` transport that declares one. */
  readonly sse?: ClientSseTransport;
}

/**
 * How a first-party SSE stream continues after its connection breaks. Mirrors
 * `SSETransport` in `protocols/clientcontract` (ADR 0013). Valid only on a safe
 * server stream; the consumer half is the effective
 * `resilience.stream.reconnect`.
 */
export interface ClientSseTransport {
  readonly continuation: ClientSseContinuation;
}

/**
 * `cursor` reopens after the position of the last delivered message, which
 * the provider continues exclusively after on any instance. `best-effort`
 * reopens the original selector with no position: messages may be missing or
 * repeated.
 */
export type ClientSseContinuation =
  | { readonly mode: 'cursor'; readonly cursor: ClientSseCursor }
  | { readonly mode: 'best-effort'; readonly cursor?: never };

/**
 * Where a cursor-mode position travels. The runtime copies it verbatim from
 * the output field to the query parameter and never parses, orders or
 * synthesizes it.
 */
export interface ClientSseCursor {
  /** Required plain-string property of every output message. */
  readonly outputField: string;
  /** Declared plain-string query parameter of the operation. */
  readonly queryParameter: string;
}

/**
 * WebSocket negotiation metadata. The first-party conversation negotiates
 * `putnami.service.v1` and omits `wire`; a provider-owned wire declares
 * `wire: 'provider'`, its own token (optional on a byte stream only) and no
 * resume. Mirrors `WebSocketTransport` in `protocols/clientcontract` (ADR 0010).
 */
export interface ClientWebSocketTransport {
  readonly subprotocol?: string;
  readonly resume: boolean;
  readonly wire?: 'provider';
}

/** Neutral schema subset shared by the TypeScript and Go client emitters. */
export interface ClientSchema {
  readonly type?: 'string' | 'number' | 'integer' | 'boolean' | 'object' | 'array';
  readonly format?:
    | 'int32'
    | 'int64'
    | 'uint32'
    | 'uint64'
    | 'float'
    | 'double'
    | 'byte'
    | 'binary'
    | 'date'
    | 'date-time'
    | 'uuid'
    | 'email'
    | 'uri';
  readonly nullable?: boolean;
  readonly $ref?: string;
  readonly properties?: Readonly<Record<string, ClientSchema>>;
  readonly required?: readonly string[];
  readonly items?: ClientSchema;
  readonly additionalProperties?: boolean | ClientSchema;
  readonly enum?: readonly (string | number | boolean | ClientExactNumber)[];
  readonly oneOf?: readonly ClientSchema[];
  readonly discriminator?: {
    readonly propertyName: string;
    readonly mapping?: Readonly<Record<string, string>>;
  };
  readonly title?: string;
  readonly description?: string;
  readonly default?: unknown;
  readonly minimum?: number | ClientExactNumber;
  readonly maximum?: number | ClientExactNumber;
  readonly exclusiveMinimum?: boolean;
  readonly exclusiveMaximum?: boolean;
  readonly minLength?: number;
  readonly maxLength?: number;
  readonly pattern?: string;
  readonly minItems?: number;
  readonly maxItems?: number;
  readonly uniqueItems?: boolean;
  readonly readOnly?: boolean;
  readonly writeOnly?: boolean;
  /**
   * Declares a value that may be any JSON document, carried without
   * interpretation. Its only value is `any`, and only `title` and
   * `description` may stand beside it. A JSON object with free-form values is
   * `type: 'object'` with `additionalProperties: true` instead.
   */
  readonly 'x-putnami-json'?: 'any';
}

/** Exact JSON number token used internally when JavaScript Number cannot represent the source lexeme. */
export interface ClientExactNumber {
  readonly $number: string;
}

export interface ClientProtobufMethod {
  readonly name: string;
  readonly input: string;
  readonly output: string;
  readonly clientStreaming: boolean;
  readonly serverStreaming: boolean;
}

export interface ClientProtobufService {
  readonly name: string;
  readonly methods: readonly ClientProtobufMethod[];
}

export interface ClientProtobufMap {
  readonly keyType: string;
  readonly valueKind: 'scalar' | 'message' | 'enum';
  readonly valueType: string;
}

export interface ClientProtobufField {
  readonly name: string;
  readonly jsonName: string;
  readonly number: number;
  readonly typeKind: 'scalar' | 'message' | 'enum' | 'map';
  readonly type: string;
  readonly repeated?: boolean;
  readonly optional?: boolean;
  readonly oneof?: string;
  readonly map?: ClientProtobufMap;
}

export interface ClientProtobufMessage {
  readonly name: string;
  readonly fields: readonly ClientProtobufField[];
  readonly oneofs?: readonly string[];
}

export interface ClientProtobufEnum {
  readonly name: string;
  readonly values: readonly { readonly name: string; readonly number: number }[];
}

/** Exact provider-owned protobuf descriptor projection used by Connect/proto transports. */
export interface ClientProtobufDescriptor {
  readonly syntax: 'proto3';
  readonly package: string;
  readonly services: readonly ClientProtobufService[];
  readonly messages: readonly ClientProtobufMessage[];
  readonly enums: readonly ClientProtobufEnum[];
}

/** One stable, declared framework error variant. */
export interface ClientDeclaredError {
  readonly status: number;
  readonly code: string;
  readonly grpcCode?: number;
  readonly schema?: ClientSchema;
  readonly retryable?: boolean;
}

/** Retry hints owned by a provider declaration. */
export interface ClientRetryPolicy {
  readonly maxAttempts?: number;
  readonly statuses?: readonly number[];
  readonly codes?: readonly string[];
}

/** Circuit-breaker hints owned by a provider declaration. */
export interface ClientCircuitPolicy {
  readonly failureThreshold?: number;
  readonly resetTimeoutMs?: number;
}

/** Streaming lifecycle hints owned by a provider declaration. */
export interface ClientStreamPolicy {
  /**
   * Bounds connection opening through admission on its own budget, separately
   * from the per-attempt timeout a unary call uses. Absent, it falls back to
   * `attemptTimeoutMs` — which is what both runtimes already did implicitly,
   * made declarable. Mirrors `resilience.stream.handshakeTimeoutMs` in
   * `protocols/clientcontract`.
   */
  readonly handshakeTimeoutMs?: number;
  readonly idleTimeoutMs?: number;
  readonly heartbeatMs?: number;
  readonly reconnect?: boolean;
  readonly maxBufferedMessages?: number;
  readonly maxFrameBytes?: number;
}

/**
 * Per-operation response cache a generated client honors. Mirrors
 * `resilience.cache` in `protocols/clientcontract` (ADR 0007): only a unary
 * operation whose idempotency is `safe` or `idempotent` may declare it, and
 * never in document defaults. Entries are always partitioned by service
 * binding and forwarded user identity, whatever `keyFields` says — and only by
 * those: a tenant carried any other way must be declared and kept in the key.
 */
export interface ClientCachePolicy {
  /** Age in milliseconds below which a stored answer is returned without a call. */
  readonly freshMs: number;
  /**
   * Age in milliseconds below which a stored answer is returned when the call
   * fails with a transport error, retry exhaustion or an open circuit. Must
   * exceed `freshMs`; absent, a failure is never masked.
   */
  readonly staleMs?: number;
  /** Least-recently-used entry bound per service binding. Absent, 1000. */
  readonly maxEntries?: number;
  /**
   * Request fields that form the key, in key order: `path.<name>`,
   * `query.<name>`, `header.<name>`, `body`, or `body.<property>`. Absent, the
   * key is every path, query and header parameter plus the whole body.
   */
  readonly keyFields?: readonly string[];
  /**
   * Top-level properties of the JSON success body that tag a stored answer,
   * so a consumer drops every answer carrying one value with
   * `invalidateResponsesByField(field, value)` — a principal id the answer
   * names while the request named something else. Each names a string, integer
   * or boolean property, never a `byte` or `binary` string, which the runtime
   * decodes to octets; absent, answers are dropped by key prefix only
   * (ADR 0011 of protocols/clientcontract).
   */
  readonly invalidationFields?: readonly string[];
}

/** Provider hints used by both runtimes to compose the same resilience semantics. */
export interface ClientResiliencePolicy {
  readonly timeoutMs?: number;
  readonly attemptTimeoutMs?: number;
  readonly maxResponseBytes?: number;
  readonly retry?: ClientRetryPolicy;
  readonly circuit?: ClientCircuitPolicy;
  readonly stream?: ClientStreamPolicy;
  /** Per-operation response cache; see {@link ClientCachePolicy}. */
  readonly cache?: ClientCachePolicy;
}

/** Retry safety declaration for one operation. */
export interface ClientIdempotencyPolicy {
  readonly kind: 'safe' | 'idempotent' | 'non-idempotent';
  readonly keyHeader?: string;
}

/** Document-level `x-putnami-client` v1 extension. */
export interface ClientContractDocument {
  readonly protocolVersion: typeof CLIENT_CONTRACT_PROTOCOL_VERSION;
  readonly service: { readonly id: string; readonly audience: string };
  readonly credentials: Readonly<Record<string, ClientCredentialProfile>>;
  readonly defaults?: { readonly resilience?: ClientResiliencePolicy };
  readonly protobuf?: ClientProtobufDescriptor;
}

/** Provider input for `api({ client })`; the protocol version is framework-owned. */
export type ClientServiceContract = Omit<ClientContractDocument, 'protocolVersion' | 'protobuf'>;

/** Operation fields authored beside the endpoint. Wire/stream/error fields are derived from the endpoint itself. */
export interface ClientOperationPolicy {
  /** Credential alternatives; authorization constraints are derived from .secure(). */
  readonly security?: Omit<ClientSecurityPolicy, 'authorization'>;
  readonly idempotency?: ClientIdempotencyPolicy;
  readonly resilience?: ClientResiliencePolicy;
  /**
   * This operation's wire preference order.
   *
   * The framework still derives which transports the route's shape can carry;
   * a declared order reorders and narrows that derived list, and never invents
   * a transport the bound server does not serve. Absent, the framework order
   * applies. It is the single place a provider states how an operation
   * travels: a generated client dispatches in this order and never branches.
   */
  readonly transports?: readonly ClientTransportProtocol[];
  /**
   * This operation's Connect payload encoding order.
   *
   * Connect is the one transport a provider serves in more than one encoding,
   * and the contract carries one entry per encoding, so this is where a
   * provider states which one a generated client dispatches first. Like
   * `transports`, it reorders and narrows the encodings the gRPC plugin
   * serves and never invents one. Absent, the plugin order applies.
   */
  readonly connectEncodings?: readonly ('json' | 'proto')[];
  /**
   * Declares that this operation's WebSocket transport can continue a server
   * stream after the last sequence the consumer completely delivered.
   *
   * Only a safe server stream may declare it: a stream whose messages carry
   * side effects cannot be continued without either a gap or a duplicate.
   */
  readonly resume?: boolean;
  /**
   * Declares how this operation's SSE transport continues after its
   * connection breaks (ADR 0013 of protocols/clientcontract).
   *
   * `{ mode: 'cursor', cursor: { outputField, queryParameter } }`: every output
   * message carries the provider's opaque position after it in `outputField`,
   * a required plain-string property; a reopened connection sends the position
   * of the last message the consumer received in `queryParameter`, a declared
   * plain-string query parameter, and the provider continues exclusively after
   * it, on any instance. The provider owns what a cursor means and refuses a
   * stale or forged one with a typed error.
   *
   * `{ mode: 'best-effort' }`: a reopened connection sends the original query
   * and no position. Messages produced while no connection was open may be
   * missing, and the provider may repeat some.
   *
   * Only a safe server stream may declare one. A route that declares one also
   * speaks the negotiated SSE wire: a consumer that asks for it is
   * acknowledged and reads an explicit `complete` terminal.
   */
  readonly sseContinuation?: ClientSseContinuation;
  /**
   * Declares that an external authority owns this operation's wire contract,
   * and names it: `'OCI Distribution Specification v1.1'`, `'npm registry API'`.
   *
   * The route stays served and stays in the OpenAPI document, where it carries
   * `x-putnami-external-contract` instead of `x-putnami-client`. It contributes
   * no operation to the first-party contract, no protobuf method, no Connect
   * RPC and no generated client method, and it answers errors and streams the
   * way an API without a contract does: the standard, not Putnami, decides its
   * wire format.
   *
   * Its own parameters, body and responses are projected the way a document
   * without a client contract projects them: the standard owns those schemas,
   * so the first-party schema rules do not apply to them, and they never
   * become a shared component. Contract components stay first-party.
   *
   * `external` stands alone. Declared beside a non-zero option of this policy,
   * left blank, or declared on an API without `api({ client })`, it fails
   * before the route is bound and again at OpenAPI generation. A zero value
   * (`resume: false`, an empty list) is not a declaration.
   */
  readonly external?: string;
}

/** Operation-scope OpenAPI extension naming the external authority that owns an operation. */
export const EXTERNAL_CONTRACT_KEY = 'x-putnami-external-contract';

/** Whether an endpoint's client policy declares an external authority with `external`. */
export function isExternalClientPolicy(policy: ClientOperationPolicy | undefined): boolean {
  return policy?.external !== undefined;
}

/**
 * The contradiction an `external` declaration carries, or undefined when it is
 * consistent. `external` stands alone, and only means something inside a
 * first-party contract: a blank authority, a first-party client option beside
 * it, and an API that publishes no contract are refused.
 *
 * A zero-valued companion is not a declaration, so `resume: false`, an empty
 * alternatives list and an empty transport order are ignored — the same
 * reading the Go framework gives them, where a zero value and an absent field
 * are one value.
 *
 * Both the api plugin (before a route is bound) and the OpenAPI generation
 * apply this one rule, the way Go applies it in Configure and in GenerateSpec.
 */
export function externalClientPolicyError(
  policy: ClientOperationPolicy | undefined,
  firstPartyContract: boolean,
): string | undefined {
  const authority = policy?.external;
  if (authority === undefined) return undefined;
  if (typeof authority !== 'string' || !authority.trim()) {
    return "external names a blank authority; name the specification that owns the route's wire contract, for example 'OCI Distribution Specification v1.1'";
  }
  const declared: string[] = [];
  if ((policy?.security?.alternatives?.length ?? 0) > 0) declared.push('security');
  if (policy?.idempotency !== undefined) declared.push('idempotency');
  if (policy?.resilience !== undefined) declared.push('resilience');
  if ((policy?.transports?.length ?? 0) > 0) declared.push('transports');
  if ((policy?.connectEncodings?.length ?? 0) > 0) declared.push('connectEncodings');
  if (policy?.resume === true) declared.push('resume');
  if (policy?.sseContinuation !== undefined) declared.push('sseContinuation');
  if (declared.length > 0) {
    const list = declared.join(', ');
    return `external ${JSON.stringify(authority)} is declared together with ${list}; an operation an external authority owns publishes no first-party client policy, so drop ${list} or drop external`;
  }
  if (!firstPartyContract) {
    return `external ${JSON.stringify(authority)} is declared, and the API publishes no first-party client contract; external leaves a route out of the contract api({ client }) publishes`;
  }
  return undefined;
}

/** Operation-level `x-putnami-client` v1 extension. */
export interface ClientContractOperation {
  readonly stream: 'unary' | 'server' | 'client' | 'bidirectional';
  readonly messages?: ClientMessageShapes;
  readonly transports: readonly ClientTransportContract[];
  readonly security: ClientSecurityPolicy;
  readonly errors: readonly ClientDeclaredError[];
  readonly idempotency: ClientIdempotencyPolicy;
  readonly resilience?: ClientResiliencePolicy;
}

/** Logical stream message schemas; websocket upgrades do not invent HTTP response bodies. */
export interface ClientMessageShapes {
  readonly input?: ClientSchema;
  readonly output?: ClientSchema;
}
