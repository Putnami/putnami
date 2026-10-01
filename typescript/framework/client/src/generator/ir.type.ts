/**
 * Intermediate representation for code generation.
 * Both OpenAPI and Proto readers produce this IR, and language-specific
 * generators consume it to produce client code.
 */

import type { ClientContractDocument, ClientContractOperation, ClientSchema } from '@putnami/application';

/** Version of the neutral, language-independent client generation IR. */
export const CLIENT_IR_VERSION = 1 as const;

/** An OpenAPI parameter with its wire location and complete neutral schema. */
export interface ParameterIR {
  name: string;
  location: 'path' | 'query' | 'header';
  required: boolean;
  schema: ClientSchema;
}

/** One media type representation for a request or response body. */
export interface ContentIR {
  mediaType: string;
  schema?: ClientSchema;
  /**
   * Declared byte bound of a raw octet representation, read from the media
   * type's `x-putnami-max-bytes`. Absent on every JSON representation, and
   * absent for a streamed binary body whose reader is handed to the caller.
   */
  maxBytes?: number;
  /** Raw unframed HTTP stream; mediaType is wildcard and maxBytes is absent. */
  streamed?: boolean;
}

/** Request body metadata. An empty content array is invalid in strict mode. */
export interface RequestIR {
  required: boolean;
  content: ContentIR[];
}

/** One declared response header. */
export interface ResponseHeaderIR {
  name: string;
  required: boolean;
  schema: ClientSchema;
}

/** One successful HTTP result variant. All declared 2xx variants are retained. */
export interface SuccessIR {
  status: number;
  description: string;
  content: ContentIR[];
  headers?: ResponseHeaderIR[];
}

/**
 * A complete service definition parsed from an API spec.
 */
export interface ServiceIR {
  /** Service name (e.g. "UsersService") */
  name: string;
  /** Generated client class name (e.g. "UsersClient") */
  className: string;
  /** RPC/endpoint methods in this service */
  methods: MethodIR[];
}

/**
 * A single method (endpoint / RPC) on a service.
 */
export interface MethodIR {
  /** Method name in camelCase (e.g. "getById") */
  name: string;
  /** Original operation ID from the spec */
  operationId: string;
  /** HTTP method (GET, POST, PUT, DELETE, PATCH) */
  httpMethod: string;
  /** URL path with {param} placeholders (e.g. "/users/{id}") */
  path: string;
  /** All path, query, and header parameters with their exact schemas. */
  parameters?: ParameterIR[];
  /** Exact request body, including root arrays/primitives and requiredness. */
  request?: RequestIR;
  /** Every declared 2xx response, including true void responses. */
  successes?: SuccessIR[];
  /** First-party operation semantics. Present only for marked contracts. */
  client?: ClientContractOperation;
  /** Path parameter types */
  params?: FieldIR[];
  /** Query parameter types */
  query?: FieldIR[];
  /** Inline request body fields (anonymous body shape). Mutually exclusive with {@link bodyType}. */
  body?: FieldIR[];
  /**
   * Named request body type — set when the body is a `$ref` to a shared model
   * in {@link SpecIR.namedTypes}. The generated method takes this type directly
   * instead of a per-operation `…Body` interface.
   */
  bodyType?: string;
  /** Inline response fields (anonymous response shape). Mutually exclusive with {@link responseType}. */
  response?: FieldIR[];
  /**
   * Named response type — set when the chosen 2xx response is a `$ref` to a
   * shared model in {@link SpecIR.namedTypes}. The method returns this type
   * directly instead of a per-operation `…Response` interface.
   */
  responseType?: string;
  /** Streaming mode if applicable */
  streaming?: 'server' | 'client' | 'bidirectional';
}

/**
 * A single field in a request/response type.
 */
export interface FieldIR {
  /** Field name in camelCase */
  name: string;
  /**
   * TypeScript type string. Either a primitive (`"string"`, `"number"`,
   * `"boolean"`), the name of a shared model in {@link SpecIR.namedTypes} (when
   * the field is a `$ref`), or `"Record<string, unknown>"` for an inline nested
   * object that was not promoted to a named type (the degrade rule).
   */
  tsType: string;
  /** Whether this field is optional */
  optional: boolean;
  /** Whether this field is an array */
  array: boolean;
}

/** A tagged union lifted from an OpenAPI oneOf component. */
export interface UnionIR {
  discriminator: string;
  variants: UnionVariantIR[];
}

/** One discriminator value and its payload fields. */
export interface UnionVariantIR {
  tag: string;
  fields?: FieldIR[];
}

/**
 * Full output of a spec reader — all services plus metadata.
 */
export interface SpecIR {
  /** Neutral IR protocol version. */
  irVersion: typeof CLIENT_IR_VERSION;
  /** Strict first-party service contract. */
  contract?: ClientContractDocument;
  /** Exact neutral component schemas keyed by OpenAPI component name. */
  schemas?: Record<string, ClientSchema>;
  /** Detected transport mode */
  transport: 'http' | 'connect';
  /** Proto package name (only for Connect transport) */
  packageName?: string;
  /** All service definitions */
  services: ServiceIR[];
  /**
   * Shared named types, keyed by name (e.g. `"Model1"`). Sourced from
   * `components.schemas` — an object model reused across operations becomes one
   * entry here and is referenced by name from method body/response types and
   * from {@link FieldIR.tsType}, instead of being inlined per operation.
   */
  namedTypes?: Record<string, FieldIR[]>;
  /** Closed string enums lifted from components.schemas. */
  enums?: Record<string, string[]>;
  /** Tagged oneOf components, preserving discriminator and variant fields. */
  unions?: Record<string, UnionIR>;
  /** SHA-256 hash of the source spec (for drift detection) */
  specHash?: string;
  /** Proto message metadata (for binary encoding) */
  protoMeta?: {
    messageMeta: Record<string, { name: string; number: number; type: string; optional: boolean; repeated: boolean }[]>;
    enumTypes: string[];
  };
}
