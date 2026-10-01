import type {
  ClientContractDocument,
  ClientContractOperation,
  ClientCredentialProfile,
  ClientExactNumber,
  ClientProtobufDescriptor,
  ClientResiliencePolicy,
  ClientSchema,
  OpenApiDocument,
  OpenApiSchema,
} from '@putnami/application';
import {
  CLIENT_IR_VERSION,
  type ContentIR,
  type FieldIR,
  type MethodIR,
  type ParameterIR,
  type RequestIR,
  type ResponseHeaderIR,
  type ServiceIR,
  type SpecIR,
  type SuccessIR,
  type UnionIR,
} from './ir.type';
import { computeSpecHash, pascalCase, toCamelCase, toSafeIdentifier } from './string-utils';

export interface ReadOpenApiOptions {
  mode: 'firstParty' | 'thirdParty';
}

/**
 * Operation-scope extension naming the external authority that owns an
 * operation of a first-party document (clientcontract ADR 0011).
 */
const EXTERNAL_CONTRACT_KEY = 'x-putnami-external-contract';

export type ClientGenerationErrorCode = 'clientgen_first_party_required' | 'clientgen_unsupported_semantic';

/**
 * Closed set of contract diagnostics, identical to the Go reader's
 * `clientcontract.ValidErrorCodes`. Both readers name the same defect with the
 * same code so `fixtures/openapi/expectations.json` is one corpus, not two.
 */
export type ClientContractDiagnosticCode =
  | 'client_contract.parse_error'
  | 'client_contract.unknown_field'
  | 'client_contract.invalid_protocol_version'
  | 'client_contract.required'
  | 'client_contract.invalid_enum'
  | 'client_contract.invalid_credential'
  | 'client_contract.unknown_profile'
  | 'client_contract.duplicate'
  | 'client_contract.invalid_transport'
  | 'client_contract.invalid_security'
  | 'client_contract.invalid_error'
  | 'client_contract.invalid_idempotency'
  | 'client_contract.invalid_resilience'
  | 'client_contract.invalid_schema'
  | 'client_contract.invalid_protobuf'
  | 'client_contract.invalid_generated_manifest'
  | 'client_contract.unsupported_runtime_capability';

/** Every contract diagnostic this reader can emit, in the Go package's order. */
export const CLIENT_CONTRACT_DIAGNOSTIC_CODES: readonly ClientContractDiagnosticCode[] = [
  'client_contract.parse_error',
  'client_contract.unknown_field',
  'client_contract.invalid_protocol_version',
  'client_contract.required',
  'client_contract.invalid_enum',
  'client_contract.invalid_credential',
  'client_contract.unknown_profile',
  'client_contract.duplicate',
  'client_contract.invalid_transport',
  'client_contract.invalid_security',
  'client_contract.invalid_error',
  'client_contract.invalid_idempotency',
  'client_contract.invalid_resilience',
  'client_contract.invalid_schema',
  'client_contract.invalid_protobuf',
  'client_contract.invalid_generated_manifest',
  'client_contract.unsupported_runtime_capability',
];

export class ClientGenerationError extends Error {
  readonly code: ClientGenerationErrorCode;
  /** Contract diagnostic, set whenever the defect is one the Go reader also names. */
  readonly contractCode?: ClientContractDiagnosticCode;
  readonly operationId?: string;
  readonly method?: string;
  readonly path?: string;
  readonly field?: string;

  constructor(
    code: ClientGenerationErrorCode,
    message: string,
    context: {
      operationId?: string;
      method?: string;
      path?: string;
      field?: string;
      contractCode?: ClientContractDiagnosticCode;
    } = {},
  ) {
    const operation = context.operationId ? ` operation ${context.operationId}` : '';
    const location = context.method && context.path ? ` (${context.method.toUpperCase()} ${context.path})` : '';
    const field = context.field ? ` at ${context.field}` : '';
    super(`${message}${operation}${location}${field}`);
    this.name = 'ClientGenerationError';
    this.code = code;
    if (context.contractCode !== undefined) this.contractCode = context.contractCode;
    this.operationId = context.operationId;
    this.method = context.method;
    this.path = context.path;
    this.field = context.field;
  }
}

interface OperationContext {
  operationId: string;
  method: string;
  path: string;
}

type StrictObjectProperty =
  | '$ref'
  | 'additionalProperties'
  | 'allOf'
  | 'alternatives'
  | 'attemptTimeoutMs'
  | 'audience'
  | 'authorization'
  | 'cache'
  | 'circuit'
  | 'clientStreaming'
  | 'code'
  | 'codes'
  | 'content'
  | 'credentials'
  | 'default'
  | 'defaults'
  | 'description'
  | 'discriminator'
  | 'encoding'
  | 'enum'
  | 'enums'
  | 'errors'
  | 'failureThreshold'
  | 'fields'
  | 'format'
  | 'freshMs'
  | 'grpcCode'
  | 'header'
  | 'headers'
  | 'id'
  | 'idempotency'
  | 'in'
  | 'input'
  | 'invalidationFields'
  | 'items'
  | 'jsonName'
  | 'keyFields'
  | 'keyHeader'
  | 'keyType'
  | 'kind'
  | 'map'
  | 'mapping'
  | 'maxAttempts'
  | 'maxEntries'
  | 'messages'
  | 'methods'
  | 'name'
  | 'number'
  | 'oneOf'
  | 'oneof'
  | 'oneofs'
  | 'optional'
  | 'output'
  | 'package'
  | 'parameters'
  | 'path'
  | 'profile'
  | 'properties'
  | 'propertyName'
  | 'protobuf'
  | 'protobufMethod'
  | 'protocol'
  | 'protocolVersion'
  | 'reconnect'
  | 'repeated'
  | 'required'
  | 'resilience'
  | 'resetTimeoutMs'
  | 'retry'
  | 'retryable'
  | 'schema'
  | 'scopes'
  | 'security'
  | 'serverStreaming'
  | 'service'
  | 'services'
  | 'staleMs'
  | 'statuses'
  | 'status'
  | 'stream'
  | 'syntax'
  | 'timeoutMs'
  | 'transports'
  | 'type'
  | 'typeKind'
  | 'valueKind'
  | 'values'
  | 'valueType';

type StrictObject = Record<string, unknown> & Partial<Record<StrictObjectProperty, unknown>>;
const HTTP_OPERATION_KEYS = new Set(['get', 'post', 'put', 'patch', 'delete', 'head', 'options', 'trace']);

const EXACT_NUMBER = Symbol('putnami.client.exact-number');

/** A JSON numeric lexeme whose integer value cannot be represented by JavaScript Number. */
export class ExactJsonNumber implements ClientExactNumber {
  readonly [EXACT_NUMBER] = true;

  constructor(readonly $number: string) {
    Object.freeze(this);
  }
}

function isExactJsonNumber(value: unknown): value is ExactJsonNumber {
  return value instanceof ExactJsonNumber && value[EXACT_NUMBER] === true;
}

/**
 * Parse raw OpenAPI bytes before semantic validation. This is the production
 * first-party entry point: duplicate keys fail and wide integer lexemes remain
 * exact instead of passing through JSON.parse/IEEE-754.
 */
export function readOpenApiSource(source: string, options: ReadOpenApiOptions): SpecIR {
  const doc = new StrictJsonParser(source).parse() as OpenApiDocument;
  const ir = readOpenApiSpecWithOptions(doc, options);
  return { ...ir, specHash: computeSpecHash(source) };
}

/** Serialize neutral IR deterministically while restoring exact-number tokens as JSON numbers. */
export function serializeClientIR(value: SpecIR): string {
  return serializeCanonicalJson(value);
}

/**
 * Serialize only the shared strict OpenAPI IR. Language-token compatibility
 * fields and source-local hashes are deliberately excluded from the parity
 * artifact consumed by the Go and TypeScript readers.
 */
export function serializeNeutralClientIR(value: SpecIR): string {
  if (!value.contract) {
    throw new TypeError('neutral client IR serialization requires a strict first-party contract');
  }
  return serializeCanonicalJson({
    irVersion: value.irVersion,
    contract: value.contract,
    ...(value.schemas ? { schemas: value.schemas } : {}),
    transport: value.transport,
    services: value.services.map((service) => ({
      name: service.name,
      className: service.className,
      methods: service.methods.map((method) => ({
        name: method.name,
        operationId: method.operationId,
        httpMethod: method.httpMethod,
        path: method.path,
        ...(method.parameters ? { parameters: method.parameters } : {}),
        ...(method.request ? { request: method.request } : {}),
        ...(method.successes ? { successes: method.successes } : {}),
        ...(method.client ? { client: method.client } : {}),
      })),
    })),
  });
}

export function readOpenApiSpec(doc: OpenApiDocument): SpecIR {
  return readOpenApiSpecWithOptions(doc, {
    mode: doc['x-putnami-client'] === undefined ? 'thirdParty' : 'firstParty',
  });
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: strict parsing keeps each rejection beside its field context
export function readOpenApiSpecWithOptions(doc: OpenApiDocument, options: ReadOpenApiOptions): SpecIR {
  const marker = doc['x-putnami-client'];
  if (options.mode === 'firstParty' && marker === undefined) {
    throw new ClientGenerationError(
      'clientgen_first_party_required',
      'provider client generation requires x-putnami-client; explicit thirdParty mode is only for external contracts',
      { field: 'x-putnami-client' },
    );
  }
  if (marker === undefined) return readThirdPartyOpenApiSpec(doc);

  const contract = parseContract(marker);
  const serviceMap = new Map<string, MethodIR[]>();
  const operationIds = new Set<string>();
  for (const [path, pathItem] of Object.entries(doc.paths).sort(([left], [right]) => compareStrings(left, right))) {
    const pathRecord = pathItem as unknown as StrictObject;
    assertKnownKeys(
      pathRecord,
      [...HTTP_OPERATION_KEYS, 'parameters', 'summary', 'description'],
      undefined,
      `paths.${path}`,
    );
    const inheritedParameters = pathRecord.parameters;
    for (const httpMethod of HTTP_OPERATION_KEYS) {
      const operationInput = pathRecord[httpMethod];
      if (operationInput === undefined) continue;
      const operation = object(
        operationInput,
        undefined,
        `paths.${path}.${httpMethod}`,
      ) as unknown as OpenApiDocument['paths'][string][string];
      const method = httpMethod.toUpperCase();
      const operationId = operation.operationId ?? buildOperationId(method, path);
      const context = { operationId, method, path };
      validateOperationObject(operation as unknown as Record<string, unknown>, context);
      // An operation an external authority owns is served and documented,
      // never generated: it contributes no method, and its own schemas are the
      // standard's, not the first-party subset.
      if (externalContractAuthority(operation as unknown as StrictObject, context) !== undefined) continue;
      if (operationIds.has(operationId))
        unsupported('duplicate operationId', context, 'operationId', 'client_contract.duplicate');
      operationIds.add(operationId);
      const parsed = strictOperationToMethod(path, method, operation, inheritedParameters, doc, contract, context);
      const serviceName = inferServiceName(path);
      const methods = serviceMap.get(serviceName) ?? [];
      methods.push(parsed);
      serviceMap.set(serviceName, methods);
    }
  }
  const services = [...serviceMap]
    .sort(([left], [right]) => compareStrings(left, right))
    .map(
      ([name, methods]): ServiceIR => ({
        name,
        className: name.replace(/Service$/, 'Client'),
        methods,
      }),
    );
  for (const service of services) {
    const generatedNames = new Set<string>();
    for (const method of service.methods) {
      if (generatedNames.has(method.name)) {
        unsupported(
          'operationIds collide after TypeScript identifier normalization',
          {
            operationId: method.operationId,
            method: method.httpMethod,
            path: method.path,
          },
          'operationId',
          'client_contract.duplicate',
        );
      }
      generatedNames.add(method.name);
    }
  }
  const schemas = readStrictComponents(doc);
  return {
    irVersion: CLIENT_IR_VERSION,
    transport: 'http',
    contract,
    services,
    ...(schemas ? { schemas } : {}),
    specHash: computeSpecHash(JSON.stringify(doc)),
  };
}

function validateOperationObject(operation: Record<string, unknown>, context: OperationContext): void {
  assertKnownKeys(
    operation,
    [
      'operationId',
      'summary',
      'description',
      'tags',
      'deprecated',
      'parameters',
      'requestBody',
      'responses',
      'security',
      'x-putnami-client',
      EXTERNAL_CONTRACT_KEY,
    ],
    context,
    `paths.${context.path}.${context.method.toLowerCase()}`,
  );
}

/**
 * The external authority that owns one operation of a first-party document, or
 * undefined for a first-party operation, which still requires
 * `x-putnami-client`. Mirrors the Go reader's
 * `clientcontract.ExternalContractAuthority`, with the same diagnostic codes.
 */
function externalContractAuthority(operation: StrictObject, context: OperationContext): string | undefined {
  const marker = operation[EXTERNAL_CONTRACT_KEY];
  if (marker === undefined) return undefined;
  if (operation['x-putnami-client'] !== undefined) {
    unsupported(
      `an operation declares both x-putnami-client and ${EXTERNAL_CONTRACT_KEY}; a first-party operation carries only x-putnami-client, and an operation an external authority owns carries only ${EXTERNAL_CONTRACT_KEY}`,
      context,
      EXTERNAL_CONTRACT_KEY,
      'client_contract.duplicate',
    );
  }
  if (typeof marker !== 'string') {
    unsupported(
      `${EXTERNAL_CONTRACT_KEY} must be a JSON string naming the external authority`,
      context,
      EXTERNAL_CONTRACT_KEY,
    );
  }
  if (!marker.trim()) {
    unsupported(
      `${EXTERNAL_CONTRACT_KEY} must name the external authority that owns the operation's wire contract`,
      context,
      EXTERNAL_CONTRACT_KEY,
      'client_contract.required',
    );
  }
  return marker;
}

function strictOperationToMethod(
  path: string,
  httpMethod: string,
  operation: OpenApiDocument['paths'][string][string],
  inheritedParameters: unknown,
  doc: OpenApiDocument,
  contract: ClientContractDocument,
  context: OperationContext,
): MethodIR {
  const extension = operation['x-putnami-client'];
  if (extension === undefined)
    unsupported(
      'first-party operation is missing client metadata',
      context,
      'x-putnami-client',
      'client_contract.required',
    );
  const client = parseOperation(extension, contract, doc, context);
  const parameters = mergeParameters(
    readStrictParameters(inheritedParameters, doc, context, 'pathParameters'),
    readStrictParameters(operation.parameters, doc, context, 'parameters'),
  );
  const request = readStrictRequest(operation.requestBody, doc, context);
  validateCacheKeyFields(client, parameters, request, doc, context);
  validateSseContinuationReferences(client, parameters, doc, context);
  const successes = readStrictSuccesses(
    operation.responses,
    doc,
    context,
    client.stream === 'client' || client.stream === 'bidirectional',
  );
  validateCacheInvalidationFields(client, successes, doc, context);
  if (
    client.resilience?.cache &&
    [...(request?.content ?? []), ...successes.flatMap((success) => success.content)].some(
      (content) => content.streamed,
    )
  ) {
    unsupported('a raw HTTP stream cannot declare a response cache', context, 'x-putnami-client.resilience.cache');
  }
  return {
    name: toSafeIdentifier(toCamelCase(context.operationId)),
    operationId: context.operationId,
    httpMethod,
    path,
    ...(parameters.length ? { parameters } : {}),
    ...(request ? { request } : {}),
    successes,
    client,
  };
}

function readStrictParameters(
  input: unknown,
  doc: OpenApiDocument,
  context: OperationContext,
  rootField: string,
): ParameterIR[] {
  if (input === undefined) return [];
  if (!Array.isArray(input)) unsupported('parameters must be an array', context, rootField);
  const seen = new Set<string>();
  return input.map((entry, index) => {
    const field = `${rootField}.${index}`;
    const parameter = object(entry, context, field);
    assertKnownKeys(parameter, ['name', 'in', 'required', 'schema', 'description'], context, field);
    if (parameter.in !== 'path' && parameter.in !== 'query' && parameter.in !== 'header') {
      unsupported(`parameter location ${JSON.stringify(parameter.in)} is unsupported`, context, `${field}.in`);
    }
    const required =
      parameter.required === undefined ? false : boolean(parameter.required, context, `${field}.required`);
    if (parameter.in === 'path' && !required)
      unsupported('path parameters must be required', context, `${field}.required`);
    const result: ParameterIR = {
      name: string(parameter.name, context, `${field}.name`),
      location: parameter.in,
      required,
      schema: readSchema(parameter.schema, doc, context, `${field}.schema`),
    };
    if (result.location === 'header') {
      validateHeader(result.name, context, `${field}.name`, false);
      if (isFrameworkReservedHeader(result.name)) {
        unsupported('first-party header parameter is framework-owned', context, `${field}.name`);
      }
    }
    const key = `${result.location}\0${result.name}`;
    if (seen.has(key)) unsupported('duplicate parameter', context, field);
    seen.add(key);
    return result;
  });
}

function mergeParameters(inherited: ParameterIR[], own: ParameterIR[]): ParameterIR[] {
  const ownKeys = new Set(own.map((parameter) => `${parameter.location}\0${parameter.name}`));
  return [...inherited.filter((parameter) => !ownKeys.has(`${parameter.location}\0${parameter.name}`)), ...own].sort(
    (left, right) =>
      left.location === right.location
        ? compareStrings(left.name, right.name)
        : parameterLocationOrder(left.location) - parameterLocationOrder(right.location),
  );
}

function parameterLocationOrder(location: ParameterIR['location']): number {
  return { path: 0, query: 1, header: 2 }[location];
}

function readStrictRequest(input: unknown, doc: OpenApiDocument, context: OperationContext): RequestIR | undefined {
  if (input === undefined) return undefined;
  const request = object(input, context, 'requestBody');
  assertKnownKeys(request, ['required', 'content', 'description'], context, 'requestBody');
  return {
    required: request.required === undefined ? false : boolean(request.required, context, 'requestBody.required'),
    content: readContent(request.content, doc, context, 'requestBody.content', true),
  };
}

function readStrictSuccesses(
  input: unknown,
  doc: OpenApiDocument,
  context: OperationContext,
  allowUpgradeOnly: boolean,
): SuccessIR[] {
  const responses = object(input, context, 'responses');
  const successes: SuccessIR[] = [];
  for (const [statusText, entry] of Object.entries(responses)) {
    if (!/^\d{3}$/.test(statusText)) {
      if (/^2/i.test(statusText))
        unsupported('success status must be a concrete numeric code', context, `responses.${statusText}`);
      continue;
    }
    const status = Number(statusText);
    if (status < 200 || status > 299) continue;
    const field = `responses.${statusText}`;
    const response = object(entry, context, field);
    assertKnownKeys(response, ['description', 'content', 'headers'], context, field);
    const content =
      response.content === undefined ? [] : readContent(response.content, doc, context, `${field}.content`, false);
    const headers = readResponseHeaders(response.headers, doc, context, `${field}.headers`);
    successes.push({
      status,
      description: string(response.description, context, `${field}.description`, true),
      content,
      ...(headers.length ? { headers } : {}),
    });
  }
  successes.sort((left, right) => left.status - right.status);
  if (!successes.length && !allowUpgradeOnly)
    unsupported('operation must declare at least one concrete 2xx response', context, 'responses');
  return successes;
}

function readResponseHeaders(
  input: unknown,
  doc: OpenApiDocument,
  context: OperationContext,
  field: string,
): ResponseHeaderIR[] {
  if (input === undefined) return [];
  return Object.entries(object(input, context, field))
    .sort(([left], [right]) => compareStrings(left, right))
    .map(([name, entry]) => {
      const headerField = `${field}.${name}`;
      const header = object(entry, context, headerField);
      assertKnownKeys(header, ['required', 'schema', 'description'], context, headerField);
      return {
        name,
        required: header.required === true,
        schema: readSchema(header.schema, doc, context, `${headerField}.schema`),
      };
    });
}

function readContent(
  input: unknown,
  doc: OpenApiDocument,
  context: OperationContext,
  field: string,
  requireEntry: boolean,
): ContentIR[] {
  const entries = Object.entries(object(input, context, field)).sort(([left], [right]) => compareStrings(left, right));
  if (requireEntry && !entries.length) unsupported('content must contain at least one media type', context, field);
  return entries.map(([mediaType, entry]) => {
    if (!mediaType.trim()) unsupported('media type must not be empty', context, field);
    const media = object(entry, context, `${field}.${mediaType}`);
    assertKnownKeys(media, ['schema', BINARY_MAX_BYTES_KEY, BINARY_STREAMED_KEY], context, `${field}.${mediaType}`);
    if (media.schema === undefined) {
      if (media[BINARY_STREAMED_KEY] !== undefined || media[BINARY_MAX_BYTES_KEY] !== undefined)
        unsupported('raw octet extensions require a binary schema', context, `${field}.${mediaType}`);
      return { mediaType };
    }
    const declaresOctets = isBinarySchemaObject(media.schema);
    const schema = readSchema(media.schema, doc, context, `${field}.${mediaType}.schema`, declaresOctets);
    if (!declaresOctets) {
      if (media[BINARY_MAX_BYTES_KEY] !== undefined || media[BINARY_STREAMED_KEY] !== undefined)
        unsupported(
          `${BINARY_MAX_BYTES_KEY} describes raw octets only`,
          context,
          `${field}.${mediaType}.${BINARY_MAX_BYTES_KEY}`,
        );
      return { mediaType, schema };
    }
    if (media[BINARY_STREAMED_KEY] !== undefined) {
      if (media[BINARY_STREAMED_KEY] !== true || mediaType !== '*/*')
        unsupported(
          'streamed octets require wildcard media type, x-putnami-streamed: true and a positive byte bound',
          context,
          `${field}.${mediaType}`,
        );
      validateOctetSchema(media, context, `${field}.${mediaType}`);
      return {
        mediaType,
        schema,
        maxBytes: readOctetBound(media, mediaType, context, `${field}.${mediaType}`, true),
        streamed: true,
      };
    }
    return { mediaType, schema, maxBytes: readOctetBound(media, mediaType, context, `${field}.${mediaType}`) };
  });
}

/** The media-type extension carrying a raw octet payload's declared bound. */
const BINARY_MAX_BYTES_KEY = 'x-putnami-max-bytes';
const BINARY_STREAMED_KEY = 'x-putnami-streamed';

/** A raw octet representation is exactly `{ type: "string", format: "binary" }`. */
function isBinarySchemaObject(input: unknown): boolean {
  if (typeof input !== 'object' || input === null) return false;
  const schema = input as Record<string, unknown>;
  return schema['type'] === 'string' && schema['format'] === 'binary';
}

/**
 * Validate the one legitimate raw octet position: the root schema of a
 * non-JSON media type, carrying a strictly positive bound and no other schema
 * vocabulary.
 */
function readOctetBound(
  media: StrictObject,
  mediaType: string,
  context: OperationContext,
  field: string,
  allowWildcard = false,
): number {
  if (!allowWildcard && !/^[\w.+-]+\/[\w.+-]+$/.test(mediaType))
    unsupported('bounded octets require a concrete media type', context, field);
  const lower = mediaType.toLowerCase();
  if (lower === 'application/json' || lower.endsWith('+json'))
    unsupported('raw octets are not representable under a JSON media type', context, field);
  validateOctetSchema(media, context, field);
  const bound = media[BINARY_MAX_BYTES_KEY];
  if (typeof bound !== 'number' || !Number.isSafeInteger(bound) || bound <= 0)
    unsupported(
      `raw octets require a positive ${BINARY_MAX_BYTES_KEY} bound`,
      context,
      `${field}.${BINARY_MAX_BYTES_KEY}`,
    );
  return bound as number;
}

function validateOctetSchema(media: StrictObject, context: OperationContext, field: string): void {
  const schema = media.schema as Record<string, unknown>;
  for (const key of Object.keys(schema)) {
    if (key !== 'type' && key !== 'format' && key !== 'description' && key !== 'title')
      unsupported('raw octets cannot be constrained with JSON schema vocabulary', context, `${field}.schema.${key}`);
  }
}

function readStrictComponents(doc: OpenApiDocument): Record<string, ClientSchema> | undefined {
  const components = doc.components?.schemas;
  if (!components || !Object.keys(components).length) return undefined;
  return Object.fromEntries(
    Object.entries(components)
      .sort(([left], [right]) => compareStrings(left, right))
      .map(([name, schema]) => [name, readSchema(schema, doc, undefined, `components.schemas.${name}`)]),
  );
}

function compareStrings(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

/** The schema keyword that declares an opaque JSON value (clientcontract ADR 0008). */
const OPAQUE_JSON_KEY = 'x-putnami-json';
/** The keywords that may stand beside it: they document the value and change nothing a client decodes. */
const OPAQUE_JSON_SIBLINGS = new Set([OPAQUE_JSON_KEY, 'title', 'description']);

const SCHEMA_KEYS = [
  'type',
  'format',
  'nullable',
  '$ref',
  'properties',
  'required',
  'items',
  'additionalProperties',
  'enum',
  'oneOf',
  'discriminator',
  'title',
  'description',
  'default',
  'minimum',
  'maximum',
  'exclusiveMinimum',
  'exclusiveMaximum',
  'minLength',
  'maxLength',
  'pattern',
  'minItems',
  'maxItems',
  'uniqueItems',
  'readOnly',
  'writeOnly',
  OPAQUE_JSON_KEY,
] as const;
const SCHEMA_TYPES = new Set(['string', 'number', 'integer', 'boolean', 'object', 'array']);
const SCHEMA_FORMATS = new Set([
  'int32',
  'int64',
  'uint32',
  'uint64',
  'float',
  'double',
  'byte',
  'binary',
  'date',
  'date-time',
  'uuid',
  'email',
  'uri',
]);

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: the closed schema subset is checked keyword by keyword
function readSchema(
  input: unknown,
  doc: OpenApiDocument,
  context: OperationContext | undefined,
  field: string,
  binaryRoot = false,
): ClientSchema {
  const schema = object(input, context, field);
  assertKnownKeys(schema, SCHEMA_KEYS, context, field);
  if (schema[OPAQUE_JSON_KEY] !== undefined) return readOpaqueJsonSchema(schema, context, field);
  if (schema.type !== undefined && (typeof schema.type !== 'string' || !SCHEMA_TYPES.has(schema.type)))
    unsupported('unsupported schema type', context, `${field}.type`);
  if (schema.format !== undefined && (typeof schema.format !== 'string' || !SCHEMA_FORMATS.has(schema.format)))
    unsupported('unsupported schema format', context, `${field}.format`);
  // `format: binary` is only representable where the HTTP body itself is the
  // payload. JSON has no octet literal, so accepting it inside a document
  // would force the reader to invent an encoding — and the two languages would
  // invent different ones. Base64 bytes inside JSON stay `format: byte`.
  if (schema.format === 'binary' && !binaryRoot)
    unsupported(
      'raw octets are not representable inside a JSON document; declare base64 bytes as format "byte", or declare the whole body binary with its own media type',
      context,
      `${field}.format`,
    );
  if (schema.$ref !== undefined) {
    const ref = string(schema.$ref, context, `${field}.$ref`);
    if (!ref.startsWith('#/components/schemas/') || !doc.components?.schemas?.[refName(ref)])
      unsupported('only existing local component references are supported', context, `${field}.$ref`);
    for (const key of Object.keys(schema)) {
      if (!['$ref', 'nullable', 'title', 'description'].includes(key))
        unsupported('$ref permits only nullable, title, and description siblings', context, `${field}.${key}`);
    }
  } else if (schema.type === undefined && schema.oneOf === undefined)
    // The empty schema stays refused: "any JSON value" is a declaration
    // (x-putnami-json), never the absence of one.
    unsupported(
      `schema must declare type, $ref, oneOf, or ${OPAQUE_JSON_KEY}`,
      context,
      field,
      'client_contract.invalid_schema',
    );
  if (schema.type === 'array' && schema.items === undefined)
    unsupported('array schema requires items', context, `${field}.items`);
  if (
    schema.type !== 'array' &&
    ['items', 'minItems', 'maxItems', 'uniqueItems'].some((key) => schema[key] !== undefined)
  )
    unsupported('array fields require array type', context, field);
  if (
    schema.type !== 'object' &&
    ['properties', 'required', 'additionalProperties'].some((key) => schema[key] !== undefined)
  )
    unsupported('object fields require object type', context, field);
  if (schema.type !== 'string' && ['minLength', 'maxLength', 'pattern'].some((key) => schema[key] !== undefined))
    unsupported('string constraints require string type', context, field);
  if (
    schema.type !== 'number' &&
    schema.type !== 'integer' &&
    ['minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum'].some((key) => schema[key] !== undefined)
  )
    unsupported('numeric constraints require number or integer type', context, field);
  if (
    schema.type === 'object' &&
    schema.properties !== undefined &&
    schema.additionalProperties !== undefined &&
    typeof schema.additionalProperties !== 'boolean'
  )
    unsupported(
      'named properties and a typed additionalProperties value cannot both be represented',
      context,
      field,
      'client_contract.invalid_schema',
    );
  validateSchemaScalars(schema, context, field);
  validateIntegerWidth(schema, context, field);
  if (schema.properties !== undefined) {
    const properties = object(schema.properties, context, `${field}.properties`);
    for (const [name, child] of Object.entries(properties))
      readSchema(child, doc, context, `${field}.properties.${name}`);
    if (schema.required !== undefined) {
      const required = strings(schema.required, context, `${field}.required`);
      unique(required, context, `${field}.required`);
      for (const name of required)
        if (!(name in properties))
          unsupported(`required property ${JSON.stringify(name)} is not declared`, context, `${field}.required`);
    }
  }
  if (schema.items !== undefined) readSchema(schema.items, doc, context, `${field}.items`);
  if (schema.additionalProperties !== undefined && typeof schema.additionalProperties !== 'boolean') {
    // One declaration, one spelling: a free-form object is
    // `additionalProperties: true`, which every OpenAPI reader understands.
    if (
      typeof schema.additionalProperties === 'object' &&
      schema.additionalProperties !== null &&
      (schema.additionalProperties as Record<string, unknown>)[OPAQUE_JSON_KEY] !== undefined
    )
      unsupported(
        `declare a free-form object as additionalProperties: true, not as an ${OPAQUE_JSON_KEY} value`,
        context,
        `${field}.additionalProperties`,
        'client_contract.invalid_schema',
      );
    readSchema(schema.additionalProperties, doc, context, `${field}.additionalProperties`);
  }
  if (schema.oneOf !== undefined) {
    if (!Array.isArray(schema.oneOf) || schema.oneOf.length < 2)
      unsupported('oneOf requires at least two variants', context, `${field}.oneOf`);
    schema.oneOf.forEach((entry, index) => {
      readSchema(entry, doc, context, `${field}.oneOf.${index}`);
    });
  }
  if (schema.discriminator !== undefined) {
    const discriminator = object(schema.discriminator, context, `${field}.discriminator`);
    assertKnownKeys(discriminator, ['propertyName', 'mapping'], context, `${field}.discriminator`);
    string(discriminator.propertyName, context, `${field}.discriminator.propertyName`);
    if (!Array.isArray(schema.oneOf) || schema.oneOf.length < 2)
      unsupported('discriminator requires oneOf', context, `${field}.discriminator`);
    if (discriminator.mapping !== undefined) {
      for (const [name, ref] of Object.entries(
        object(discriminator.mapping, context, `${field}.discriminator.mapping`),
      ))
        string(ref, context, `${field}.discriminator.mapping.${name}`);
    }
  }
  if (schema.enum !== undefined) {
    if (!Array.isArray(schema.enum) || !schema.enum.length)
      unsupported('enum must contain values', context, `${field}.enum`);
    for (const [index, value] of schema.enum.entries()) {
      if (!['string', 'number', 'boolean'].includes(typeof value) && !isExactJsonNumber(value))
        unsupported('unsupported enum value', context, `${field}.enum.${index}`);
      if (typeof value === 'number' || isExactJsonNumber(value)) {
        validateJsonNumber(value, context, `${field}.enum.${index}`);
      }
    }
  }
  if (schema.default !== undefined) validateJsonValue(schema.default, context, `${field}.default`);
  return cloneContractValue(schema) as ClientSchema;
}

/**
 * The one closed form of an opaque JSON value: the keyword with its only value,
 * and nothing that would constrain or shape the value. Null is already one of
 * the values it admits, so a nullable flag would be a second spelling.
 */
function readOpaqueJsonSchema(
  schema: StrictObject,
  context: OperationContext | undefined,
  field: string,
): ClientSchema {
  if (schema[OPAQUE_JSON_KEY] !== 'any')
    unsupported(
      `${OPAQUE_JSON_KEY} must be "any"`,
      context,
      `${field}.${OPAQUE_JSON_KEY}`,
      typeof schema[OPAQUE_JSON_KEY] === 'string' ? 'client_contract.invalid_enum' : 'client_contract.parse_error',
    );
  for (const key of Object.keys(schema)) {
    if (!OPAQUE_JSON_SIBLINGS.has(key))
      unsupported(
        `${OPAQUE_JSON_KEY} permits only title and description siblings`,
        context,
        `${field}.${key}`,
        'client_contract.invalid_schema',
      );
  }
  if (schema['title'] !== undefined) string(schema['title'], context, `${field}.title`, true);
  if (schema['description'] !== undefined) string(schema['description'], context, `${field}.description`, true);
  return cloneContractValue(schema) as ClientSchema;
}

/** Inclusive range each declared integer width can carry, as exact decimal text. */
const INTEGER_WIDTH_RANGE: Record<string, { minimum: bigint; maximum: bigint }> = {
  int32: { minimum: -2147483648n, maximum: 2147483647n },
  uint32: { minimum: 0n, maximum: 4294967295n },
  int64: { minimum: -9223372036854775808n, maximum: 9223372036854775807n },
  uint64: { minimum: 0n, maximum: 18446744073709551615n },
};

/**
 * D0.2: an integer's width is declared, not inferred. A declared bound must fit
 * the declared width — a `uint32` field cannot carry a `uint64` maximum — and it
 * is read from its exact decimal token, never through IEEE-754.
 */
function validateIntegerWidth(
  schema: Record<string, unknown>,
  context: OperationContext | undefined,
  field: string,
): void {
  if (schema['type'] !== 'integer') return;
  const format = schema['format'];
  if (format === undefined) return;
  const range = INTEGER_WIDTH_RANGE[format as string];
  if (!range)
    unsupported(
      'integer format must be int32, int64, uint32, or uint64',
      context,
      `${field}.format`,
      'client_contract.invalid_schema',
    );
  for (const key of ['minimum', 'maximum'] as const) {
    const bound = exactIntegerBound(schema[key]);
    if (bound === undefined) continue;
    if (bound < range.minimum || bound > range.maximum)
      unsupported(
        `${key} is outside the declared ${String(format)} range`,
        context,
        `${field}.${key}`,
        'client_contract.invalid_schema',
      );
  }
}

/** Read a bound as an exact integer, preferring the source token over the double. */
function exactIntegerBound(value: unknown): bigint | undefined {
  if (isExactJsonNumber(value)) {
    try {
      return BigInt(value.$number);
    } catch {
      return undefined;
    }
  }
  return typeof value === 'number' && Number.isInteger(value) ? BigInt(value) : undefined;
}

function validateSchemaScalars(
  schema: Record<string, unknown>,
  context: OperationContext | undefined,
  field: string,
): void {
  for (const key of ['nullable', 'exclusiveMinimum', 'exclusiveMaximum', 'uniqueItems', 'readOnly', 'writeOnly'])
    if (schema[key] !== undefined) boolean(schema[key], context, `${field}.${key}`);
  for (const key of ['minimum', 'maximum'])
    if (schema[key] !== undefined) validateJsonNumber(schema[key], context, `${field}.${key}`);
  for (const key of ['minLength', 'maxLength', 'minItems', 'maxItems'])
    if (schema[key] !== undefined && (!Number.isSafeInteger(schema[key]) || (schema[key] as number) < 0))
      unsupported('expected a non-negative safe integer', context, `${field}.${key}`);
  for (const key of ['title', 'description', 'pattern'])
    if (schema[key] !== undefined && typeof schema[key] !== 'string')
      unsupported('expected a string', context, `${field}.${key}`);
}

function parseContract(input: unknown): ClientContractDocument {
  const contract = object(input, undefined, 'x-putnami-client');
  assertKnownKeys(
    contract,
    ['protocolVersion', 'service', 'credentials', 'defaults', 'protobuf'],
    undefined,
    'x-putnami-client',
  );
  if (contract.protocolVersion !== 1)
    unsupported(
      'unsupported protocol version',
      undefined,
      'x-putnami-client.protocolVersion',
      'client_contract.invalid_protocol_version',
    );
  const service = object(contract.service, undefined, 'x-putnami-client.service');
  assertKnownKeys(service, ['id', 'audience'], undefined, 'x-putnami-client.service');
  string(service.id, undefined, 'x-putnami-client.service.id');
  string(service.audience, undefined, 'x-putnami-client.service.audience');
  const credentials = object(contract.credentials, undefined, 'x-putnami-client.credentials');
  for (const [name, profile] of Object.entries(credentials)) parseCredential(name, profile);
  if (contract.defaults !== undefined) {
    const defaults = object(contract.defaults, undefined, 'x-putnami-client.defaults');
    assertKnownKeys(defaults, ['resilience'], undefined, 'x-putnami-client.defaults');
    if (defaults.resilience !== undefined) {
      const resilience = validateResilience(defaults.resilience, undefined, 'x-putnami-client.defaults.resilience');
      if (resilience.cache !== undefined)
        unsupported(
          'a response cache is declared per operation; a document default would cache operations that never asked for it',
          undefined,
          'x-putnami-client.defaults.resilience.cache',
          'client_contract.invalid_resilience',
        );
    }
  }
  if (contract.protobuf !== undefined) validateProtobuf(contract.protobuf);
  return cloneContractValue(contract) as unknown as ClientContractDocument;
}

function parseCredential(name: string, input: unknown): ClientCredentialProfile {
  if (!name.trim())
    unsupported(
      'credential profile name must not be empty',
      undefined,
      'x-putnami-client.credentials',
      'client_contract.invalid_credential',
    );
  const field = `x-putnami-client.credentials.${name}`;
  const profile = object(input, undefined, field);
  assertKnownKeys(profile, ['kind', 'audience', 'scopes', 'header'], undefined, field);
  switch (profile.kind) {
    case 'service-token':
      if (profile.audience !== undefined) string(profile.audience, undefined, `${field}.audience`);
      if (profile.scopes !== undefined)
        unique(strings(profile.scopes, undefined, `${field}.scopes`), undefined, `${field}.scopes`);
      if (profile.header !== undefined)
        unsupported('service-token has no header', undefined, `${field}.header`, 'client_contract.invalid_credential');
      break;
    case 'forwarded-user-token':
      if (Object.keys(profile).length !== 1)
        unsupported(
          'forwarded-user-token has no profile fields',
          undefined,
          field,
          'client_contract.invalid_credential',
        );
      break;
    case 'api-key':
    case 'named-header':
      validateHeader(
        string(profile.header, undefined, `${field}.header`),
        undefined,
        `${field}.header`,
        true,
        'client_contract.invalid_credential',
      );
      if (profile.audience !== undefined || profile.scopes !== undefined)
        unsupported(`${profile.kind} accepts only header`, undefined, field, 'client_contract.invalid_credential');
      break;
    default:
      unsupported('unsupported credential kind', undefined, `${field}.kind`, 'client_contract.invalid_credential');
  }
  return profile as unknown as ClientCredentialProfile;
}

function parseOperation(
  input: unknown,
  contract: ClientContractDocument,
  doc: OpenApiDocument,
  context: OperationContext,
): ClientContractOperation {
  const operation = object(input, context, 'x-putnami-client');
  assertKnownKeys(
    operation,
    ['stream', 'messages', 'transports', 'security', 'errors', 'idempotency', 'resilience'],
    context,
    'x-putnami-client',
  );
  if (!['unary', 'server', 'client', 'bidirectional'].includes(operation.stream as string))
    unsupported('unsupported stream mode', context, 'x-putnami-client.stream', 'client_contract.invalid_enum');
  validateMessageShapes(
    operation.messages,
    operation.stream as ClientContractOperation['stream'],
    doc,
    context,
    declaresByteStream(operation.transports),
  );
  validateTransports(
    operation.transports,
    operation.stream as ClientContractOperation['stream'],
    contract.protobuf,
    context,
  );
  validateSecurity(operation.security, contract, context);
  validateErrors(operation.errors, doc, context);
  validateIdempotency(operation.idempotency, context);
  if (operation.resilience !== undefined) {
    const resilience = validateResilience(operation.resilience, context, 'x-putnami-client.resilience');
    if (resilience.cache !== undefined) validateCacheScope(operation, context);
  }
  validateWebsocketResume(operation, contract, context);
  return cloneContractValue(operation) as unknown as ClientContractOperation;
}

/**
 * A response cache stands in for a call only where replaying the answer is the
 * same as calling again: one request, one response, no side effect.
 */
function validateCacheScope(operation: StrictObject, context: OperationContext): void {
  if (operation.stream !== 'unary')
    unsupported(
      'a response cache requires a unary operation',
      context,
      'x-putnami-client.resilience.cache',
      'client_contract.invalid_resilience',
    );
  const kind = (operation.idempotency as { kind?: unknown } | undefined)?.kind;
  if (kind !== 'safe' && kind !== 'idempotent')
    unsupported(
      'a response cache requires a safe or idempotent operation',
      context,
      'x-putnami-client.resilience.cache',
      'client_contract.invalid_resilience',
    );
}

/**
 * Whether an operation's transports declare a provider-owned byte stream. It
 * reads defensively: the transports are validated after the message shapes,
 * and a malformed list simply declares none.
 */
function declaresByteStream(transports: unknown): boolean {
  if (!Array.isArray(transports)) return false;
  return transports.some((entry) => {
    if (typeof entry !== 'object' || entry === null) return false;
    const transport = entry as StrictObject;
    const websocket = transport['websocket'] as StrictObject | undefined;
    return (
      transport['protocol'] === 'websocket' && transport['encoding'] === 'binary' && websocket?.['wire'] === 'provider'
    );
  });
}

function validateMessageShapes(
  input: unknown,
  stream: ClientContractOperation['stream'],
  doc: OpenApiDocument,
  context: OperationContext,
  byteStream = false,
): void {
  if (byteStream) {
    // Raw octets have no schema: a message schema beside them would describe
    // a value the wire never carries.
    if (input !== undefined)
      unsupported(
        'a provider-owned byte stream carries raw octets and must omit messages',
        context,
        'x-putnami-client.messages',
        'client_contract.invalid_schema',
      );
    return;
  }
  if (stream === 'unary') {
    if (input !== undefined)
      unsupported('unary operations must omit stream messages', context, 'x-putnami-client.messages');
    return;
  }
  const messages = object(input, context, 'x-putnami-client.messages');
  assertKnownKeys(messages, ['input', 'output'], context, 'x-putnami-client.messages');
  if ((stream === 'client' || stream === 'bidirectional') && messages.input === undefined) {
    unsupported(`${stream} stream requires input message schema`, context, 'x-putnami-client.messages.input');
  }
  if ((stream === 'server' || stream === 'bidirectional') && messages.output === undefined) {
    unsupported(`${stream} stream requires output message schema`, context, 'x-putnami-client.messages.output');
  }
  if (stream === 'server' && messages.input !== undefined) {
    unsupported('server stream must not declare input message schema', context, 'x-putnami-client.messages.input');
  }
  if (messages.input !== undefined) readSchema(messages.input, doc, context, 'x-putnami-client.messages.input');
  if (messages.output !== undefined) readSchema(messages.output, doc, context, 'x-putnami-client.messages.output');
}

function validateTransports(
  input: unknown,
  stream: ClientContractOperation['stream'],
  protobuf: ClientProtobufDescriptor | undefined,
  context: OperationContext,
): void {
  if (!Array.isArray(input) || !input.length)
    unsupported('transports must contain entries', context, 'x-putnami-client.transports', 'client_contract.required');
  const seen = new Set<string>();
  let hasWebSocket = false;
  let providerWires = 0;
  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: each entry validates a closed cross-field transport state
  input.forEach((entry, index) => {
    const field = `x-putnami-client.transports.${index}`;
    const transport = object(entry, context, field);
    assertKnownKeys(transport, ['protocol', 'path', 'encoding', 'protobufMethod', 'websocket', 'sse'], context, field);
    if (transport['sse'] !== undefined && transport.protocol !== 'sse')
      unsupported(
        'sse metadata is valid only for sse transports',
        context,
        `${field}.sse`,
        'client_contract.invalid_transport',
      );
    if (!['rest-json', 'connect', 'sse', 'websocket'].includes(transport.protocol as string))
      unsupported('unsupported transport protocol', context, `${field}.protocol`, 'client_contract.invalid_enum');
    if (!['json', 'proto', 'binary'].includes(transport.encoding as string))
      unsupported('unsupported transport encoding', context, `${field}.encoding`, 'client_contract.invalid_enum');
    const path = string(transport.path, context, `${field}.path`);
    validateTransportPath(path, context, `${field}.path`);
    const needsProtobuf = transport.protocol === 'connect' || transport.encoding === 'proto';
    if (needsProtobuf) {
      if (!protobuf)
        unsupported(
          'Connect/proto transport requires document protobuf descriptor',
          context,
          field,
          'client_contract.invalid_transport',
        );
      const protobufMethod = string(transport.protobufMethod, context, `${field}.protobufMethod`);
      validateProtobufMethodReference(protobufMethod, protobuf, stream, context, `${field}.protobufMethod`);
    } else if (transport.protobufMethod !== undefined)
      unsupported(
        'protobufMethod is only valid for Connect/proto',
        context,
        `${field}.protobufMethod`,
        'client_contract.invalid_transport',
      );
    if (transport.protocol === 'rest-json' && (stream !== 'unary' || transport.encoding !== 'json'))
      unsupported('rest-json requires unary/json', context, field, 'client_contract.invalid_transport');
    if (transport.protocol === 'sse' && (stream !== 'server' || transport.encoding !== 'json'))
      unsupported('sse requires server/json', context, field, 'client_contract.invalid_transport');
    if (transport.protocol === 'sse' && transport['sse'] !== undefined)
      validateSseTransport(transport['sse'], context, `${field}.sse`);
    if (transport.protocol === 'websocket') {
      const websocket = object(transport['websocket'], context, `${field}.websocket`);
      assertKnownKeys(websocket, ['subprotocol', 'resume', 'wire'], context, `${field}.websocket`);
      if (websocket['wire'] === undefined) {
        if (transport.encoding === 'binary')
          unsupported(
            'binary payloads travel only on a provider-owned websocket wire',
            context,
            `${field}.encoding`,
            'client_contract.invalid_transport',
          );
        if (websocket['subprotocol'] !== 'putnami.service.v1')
          unsupported(
            'unsupported websocket subprotocol',
            context,
            `${field}.websocket.subprotocol`,
            'client_contract.invalid_transport',
          );
      } else if (websocket['wire'] === 'provider') {
        providerWires += 1;
        validateProviderWire(transport, websocket, stream, context, field);
      } else {
        unsupported('unsupported websocket wire', context, `${field}.websocket.wire`, 'client_contract.invalid_enum');
      }
      boolean(websocket['resume'], context, `${field}.websocket.resume`);
    } else if (transport['websocket'] !== undefined) {
      unsupported(
        'websocket metadata is only valid for websocket transport',
        context,
        `${field}.websocket`,
        'client_contract.invalid_transport',
      );
    }
    hasWebSocket ||= transport.protocol === 'websocket';
    const key = `${transport.protocol}\0${path}\0${transport.encoding}`;
    if (seen.has(key)) unsupported('duplicate transport', context, field, 'client_contract.duplicate');
    seen.add(key);
  });
  if ((stream === 'client' || stream === 'bidirectional') && !hasWebSocket)
    unsupported(
      'client streaming requires websocket transport',
      context,
      'x-putnami-client.transports',
      'client_contract.invalid_transport',
    );
  // A route serves one wire: a provider-owned wire beside any other transport
  // would ask a client to pick between two handler shapes for one operation.
  if (providerWires > 0 && input.length !== 1)
    unsupported(
      "a provider-owned websocket wire must be the operation's only transport",
      context,
      'x-putnami-client.transports',
      'client_contract.invalid_transport',
    );
}

/** The first-party conversation's namespace, which a provider-owned wire never declares. */
const RESERVED_SUBPROTOCOL_PREFIX = 'putnami.service.';
const SUBPROTOCOL_TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

/**
 * Check one provider-owned websocket wire (ADR 0010): JSON values under a
 * declared token or raw octets, on a bidirectional stream, outside the
 * first-party namespace, with no framework resume. The rules and their codes
 * are the Go reader's, in the Go reader's order.
 */
function validateProviderWire(
  transport: StrictObject,
  websocket: StrictObject,
  stream: ClientContractOperation['stream'],
  context: OperationContext,
  field: string,
): void {
  if (transport['encoding'] !== 'json' && transport['encoding'] !== 'binary')
    unsupported(
      'a provider-owned websocket wire carries json or binary messages',
      context,
      `${field}.encoding`,
      'client_contract.invalid_transport',
    );
  if (stream !== 'bidirectional')
    unsupported(
      `a provider-owned websocket wire carries a bidirectional stream, not a ${stream} one`,
      context,
      `${field}.websocket.wire`,
      'client_contract.invalid_transport',
    );
  const subprotocol = websocket['subprotocol'];
  if (subprotocol === undefined) {
    if (transport['encoding'] === 'json')
      unsupported(
        'a typed provider-owned wire must name its subprotocol',
        context,
        `${field}.websocket.subprotocol`,
        'client_contract.required',
      );
  } else {
    const token = string(subprotocol, context, `${field}.websocket.subprotocol`);
    if (!SUBPROTOCOL_TOKEN.test(token))
      unsupported(
        'websocket subprotocol is not a negotiable token',
        context,
        `${field}.websocket.subprotocol`,
        'client_contract.invalid_transport',
      );
    if (token.startsWith(RESERVED_SUBPROTOCOL_PREFIX))
      unsupported(
        'a provider-owned wire cannot declare a token in the first-party namespace',
        context,
        `${field}.websocket.subprotocol`,
        'client_contract.invalid_transport',
      );
  }
  if (websocket['resume'] === true)
    unsupported(
      "resume is the first-party conversation's continuation; a provider-owned wire resumes by its own protocol",
      context,
      `${field}.websocket.resume`,
      'client_contract.invalid_resilience',
    );
}

/**
 * Check the shape of one SSE continuation declaration (clientcontract ADR
 * 0013). The rules and their codes are the Go reader's `validateSSETransport`;
 * where a continuation may appear is checked with the operation, and what its
 * cursor names by `validateSseContinuationReferences`.
 */
function validateSseTransport(input: unknown, context: OperationContext, field: string): void {
  const sse = object(input, context, field);
  assertKnownKeys(sse, ['continuation'], context, field);
  const continuationField = `${field}.continuation`;
  if (sse['continuation'] === undefined)
    unsupported('sse metadata declares no continuation', context, continuationField, 'client_contract.required');
  const continuation = object(sse['continuation'], context, continuationField);
  assertKnownKeys(continuation, ['mode', 'cursor'], context, continuationField);
  const mode = continuation['mode'];
  if (mode === undefined || (typeof mode === 'string' && !mode.trim()))
    unsupported('sse continuation declares no mode', context, `${continuationField}.mode`, 'client_contract.required');
  if (typeof mode !== 'string') unsupported('expected a string', context, `${continuationField}.mode`);
  if (mode === 'cursor') {
    const cursorField = `${continuationField}.cursor`;
    if (continuation['cursor'] === undefined)
      unsupported('a cursor continuation names no cursor', context, cursorField, 'client_contract.required');
    const cursor = object(continuation['cursor'], context, cursorField);
    assertKnownKeys(cursor, ['outputField', 'queryParameter'], context, cursorField);
    for (const member of ['outputField', 'queryParameter'] as const) {
      const value = cursor[member];
      if (value !== undefined && typeof value !== 'string')
        unsupported('expected a string', context, `${cursorField}.${member}`);
      if (value === undefined || !(value as string).trim())
        unsupported(
          `a cursor continuation names no ${member}`,
          context,
          `${cursorField}.${member}`,
          'client_contract.required',
        );
    }
  } else if (mode === 'best-effort') {
    if (continuation['cursor'] !== undefined)
      unsupported(
        'best-effort continuation reopens the original selector and never carries a cursor',
        context,
        `${continuationField}.cursor`,
        'client_contract.invalid_resilience',
      );
  } else {
    unsupported(
      `sse continuation mode ${JSON.stringify(mode)} is unsupported`,
      context,
      `${continuationField}.mode`,
      'client_contract.invalid_enum',
    );
  }
}

/** Whether a transport is an sse transport that declares a continuation. */
function declaresSseContinuation(transport: StrictObject): boolean {
  return transport['protocol'] === 'sse' && transport['sse'] !== undefined;
}

function validateWebsocketResume(
  operation: StrictObject,
  contract: ClientContractDocument,
  context: OperationContext,
): void {
  const transports = operation.transports as StrictObject[];
  // Only a first-party transport resumes: a provider-owned wire resumes by its
  // own protocol, so it never counts as resumable.
  const resumable = transports.some((transport) => {
    const websocket = transport['websocket'] as StrictObject | undefined;
    return (
      transport['protocol'] === 'websocket' && websocket?.['wire'] !== 'provider' && websocket?.['resume'] === true
    );
  });
  const continuable = transports.some(declaresSseContinuation);
  const reconnect = streamReconnect(operation, contract);
  if (!resumable && !continuable && reconnect) {
    unsupported(
      'stream reconnect requires a websocket transport with resume support or an sse transport that declares a continuation',
      context,
      'x-putnami-client.resilience.stream.reconnect',
      'client_contract.invalid_resilience',
    );
  }
  const idempotency = operation.idempotency as StrictObject;
  for (const [index, transport] of transports.entries()) {
    if (declaresSseContinuation(transport) && (operation.stream !== 'server' || idempotency.kind !== 'safe'))
      unsupported(
        'sse continuation is valid only for safe server streams in protocol v1',
        context,
        `x-putnami-client.transports.${index}.sse.continuation`,
        'client_contract.invalid_resilience',
      );
  }
  if (!resumable) return;
  if (operation.stream !== 'server') {
    unsupported(
      'websocket resume is supported only for server streams',
      context,
      'x-putnami-client.transports',
      'client_contract.invalid_transport',
    );
  }
  if (idempotency.kind !== 'safe') {
    unsupported(
      'websocket resume requires safe idempotency',
      context,
      'x-putnami-client.idempotency.kind',
      'client_contract.invalid_idempotency',
    );
  }
  if (!reconnect) {
    unsupported(
      'websocket resume requires reconnect policy',
      context,
      'x-putnami-client.resilience.stream.reconnect',
      'client_contract.invalid_resilience',
    );
  }
}

/**
 * The operation's effective `resilience.stream.reconnect`. The operation's own
 * value always counts. The document default reaches only server streams, the
 * one mode a reconnect can continue, so a document that turns reconnect on for
 * its streams leaves its unary operations valid. The Go reader resolves it the
 * same way.
 */
function streamReconnect(operation: StrictObject, contract: ClientContractDocument): boolean {
  const operationStream = (operation.resilience as StrictObject | undefined)?.stream as StrictObject | undefined;
  if (operationStream?.reconnect !== undefined) return operationStream.reconnect === true;
  if (operation.stream !== 'server') return false;
  return contract.defaults?.resilience?.stream?.reconnect === true;
}

function validateSecurity(input: unknown, contract: ClientContractDocument, context: OperationContext): void {
  const security = object(input, context, 'x-putnami-client.security');
  assertKnownKeys(security, ['alternatives', 'authorization'], context, 'x-putnami-client.security');
  if (!Array.isArray(security.alternatives) || !security.alternatives.length)
    unsupported(
      'security alternatives must not be empty',
      context,
      'x-putnami-client.security.alternatives',
      'client_contract.invalid_security',
    );
  security.alternatives.forEach((entry, index) => {
    const field = `x-putnami-client.security.alternatives.${index}`;
    const alternative = object(entry, context, field);
    assertKnownKeys(alternative, ['allOf'], context, field);
    if (!Array.isArray(alternative.allOf))
      unsupported('allOf must be an array', context, `${field}.allOf`, 'client_contract.invalid_security');
    const profiles = new Set<string>();
    alternative.allOf.forEach((requirementEntry, requirementIndex) => {
      const requirementField = `${field}.allOf.${requirementIndex}`;
      const requirement = object(requirementEntry, context, requirementField);
      assertKnownKeys(requirement, ['profile', 'scopes', 'roles'], context, requirementField);
      const profile = string(requirement.profile, context, `${requirementField}.profile`);
      if (!contract.credentials[profile])
        unsupported(
          'unknown credential profile',
          context,
          `${requirementField}.profile`,
          'client_contract.unknown_profile',
        );
      if (profiles.has(profile))
        unsupported(
          'duplicate credential profile',
          context,
          `${requirementField}.profile`,
          'client_contract.duplicate',
        );
      profiles.add(profile);
      for (const name of ['scopes', 'roles'])
        if (requirement[name] !== undefined)
          unique(
            strings(requirement[name], context, `${requirementField}.${name}`),
            context,
            `${requirementField}.${name}`,
          );
    });
  });
  if (security.authorization !== undefined) {
    const field = 'x-putnami-client.security.authorization';
    const authorization = object(security.authorization, context, field);
    const names = [
      'issuers',
      'audiences',
      'principalKinds',
      'clients',
      'scopesAll',
      'scopesAny',
      'rolesAll',
      'rolesAny',
      'scopeClaims',
      'roleClaims',
    ];
    assertKnownKeys(authorization, names, context, field);
    for (const name of names)
      if (authorization[name] !== undefined)
        unique(strings(authorization[name], context, `${field}.${name}`), context, `${field}.${name}`);
  }
}

function validateErrors(input: unknown, doc: OpenApiDocument, context: OperationContext): void {
  if (!Array.isArray(input))
    unsupported('errors must be an array', context, 'x-putnami-client.errors', 'client_contract.invalid_error');
  const seen = new Set<string>();
  input.forEach((entry, index) => {
    const field = `x-putnami-client.errors.${index}`;
    const error = object(entry, context, field);
    assertKnownKeys(error, ['status', 'code', 'grpcCode', 'schema', 'retryable'], context, field);
    if (!Number.isInteger(error.status) || (error.status as number) < 400 || (error.status as number) > 599)
      unsupported('error status must be 400..599', context, `${field}.status`, 'client_contract.invalid_error');
    const code = string(error.code, context, `${field}.code`);
    if (
      error.grpcCode !== undefined &&
      (!Number.isInteger(error.grpcCode) || (error.grpcCode as number) < 1 || (error.grpcCode as number) > 16)
    )
      unsupported('grpcCode must be 1..16', context, `${field}.grpcCode`, 'client_contract.invalid_error');
    if (error.schema !== undefined) readSchema(error.schema, doc, context, `${field}.schema`);
    if (error.retryable !== undefined) boolean(error.retryable, context, `${field}.retryable`);
    const key = `${error.status}\0${code}\0${error.grpcCode ?? ''}`;
    if (seen.has(key)) unsupported('duplicate error', context, field, 'client_contract.duplicate');
    seen.add(key);
  });
}

function validateIdempotency(input: unknown, context: OperationContext): void {
  const policy = object(input, context, 'x-putnami-client.idempotency');
  assertKnownKeys(policy, ['kind', 'keyHeader'], context, 'x-putnami-client.idempotency');
  if (!['safe', 'idempotent', 'non-idempotent'].includes(policy.kind as string))
    unsupported(
      'unsupported idempotency kind',
      context,
      'x-putnami-client.idempotency.kind',
      'client_contract.invalid_enum',
    );
  if (policy.keyHeader !== undefined) {
    if (policy.kind !== 'idempotent')
      unsupported(
        'keyHeader requires idempotent kind',
        context,
        'x-putnami-client.idempotency.keyHeader',
        'client_contract.invalid_idempotency',
      );
    validateHeader(
      string(policy.keyHeader, context, 'x-putnami-client.idempotency.keyHeader'),
      context,
      'x-putnami-client.idempotency.keyHeader',
      false,
      'client_contract.invalid_idempotency',
    );
    if (isFrameworkReservedHeader(policy.keyHeader as string)) {
      unsupported(
        'idempotency header is framework-owned',
        context,
        'x-putnami-client.idempotency.keyHeader',
        'client_contract.invalid_idempotency',
      );
    }
  }
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: local checks retain exact nested field diagnostics
function validateResilience(
  input: unknown,
  context: OperationContext | undefined,
  field: string,
): ClientResiliencePolicy {
  const policy = object(input, context, field);
  assertKnownKeys(
    policy,
    ['timeoutMs', 'attemptTimeoutMs', 'maxResponseBytes', 'retry', 'circuit', 'stream', 'cache'],
    context,
    field,
  );
  for (const name of ['timeoutMs', 'attemptTimeoutMs', 'maxResponseBytes'])
    positive(policy[name], context, `${field}.${name}`);
  if (
    typeof policy.timeoutMs === 'number' &&
    typeof policy.attemptTimeoutMs === 'number' &&
    policy.attemptTimeoutMs > policy.timeoutMs
  )
    unsupported('attemptTimeoutMs exceeds timeoutMs', context, field, 'client_contract.invalid_resilience');
  if (policy.retry !== undefined) {
    const retry = object(policy.retry, context, `${field}.retry`);
    assertKnownKeys(retry, ['maxAttempts', 'statuses', 'codes'], context, `${field}.retry`);
    positive(retry.maxAttempts, context, `${field}.retry.maxAttempts`);
    if (retry.statuses !== undefined) {
      if (!Array.isArray(retry.statuses)) unsupported('statuses must be an array', context, `${field}.retry.statuses`);
      for (const [index, status] of retry.statuses.entries())
        if (!Number.isInteger(status) || status < 400 || status > 599)
          unsupported(
            'retry status must be 400..599',
            context,
            `${field}.retry.statuses.${index}`,
            'client_contract.invalid_resilience',
          );
    }
    if (retry.codes !== undefined)
      unique(strings(retry.codes, context, `${field}.retry.codes`), context, `${field}.retry.codes`);
  }
  if (policy.circuit !== undefined) {
    const circuit = object(policy.circuit, context, `${field}.circuit`);
    assertKnownKeys(circuit, ['failureThreshold', 'resetTimeoutMs'], context, `${field}.circuit`);
    positive(circuit.failureThreshold, context, `${field}.circuit.failureThreshold`);
    positive(circuit.resetTimeoutMs, context, `${field}.circuit.resetTimeoutMs`);
  }
  if (policy.stream !== undefined) {
    const stream = object(policy.stream, context, `${field}.stream`);
    assertKnownKeys(
      stream,
      ['handshakeTimeoutMs', 'idleTimeoutMs', 'heartbeatMs', 'reconnect', 'maxBufferedMessages', 'maxFrameBytes'],
      context,
      `${field}.stream`,
    );
    for (const name of ['handshakeTimeoutMs', 'idleTimeoutMs', 'heartbeatMs', 'maxBufferedMessages', 'maxFrameBytes'])
      positive(stream[name], context, `${field}.stream.${name}`);
    if (stream.reconnect !== undefined) boolean(stream.reconnect, context, `${field}.stream.reconnect`);
  }
  if (policy.cache !== undefined) validateCachePolicy(policy.cache, context, `${field}.cache`);
  return policy as unknown as ClientResiliencePolicy;
}

/**
 * The values of one response cache declaration, checked exactly as the Go
 * reader checks them (`validateCachePolicy` in protocols/clientcontract). Its
 * scope — unary, safe or idempotent, never a document default — is checked by
 * the callers that know the operation.
 */
function validateCachePolicy(input: unknown, context: OperationContext | undefined, field: string): void {
  const cache = object(input, context, field);
  assertKnownKeys(cache, ['freshMs', 'staleMs', 'maxEntries', 'keyFields', 'invalidationFields'], context, field);
  if (cache.freshMs === undefined) unsupported('freshMs is required', context, `${field}.freshMs`);
  positive(cache.freshMs, context, `${field}.freshMs`);
  positive(cache.staleMs, context, `${field}.staleMs`);
  positive(cache.maxEntries, context, `${field}.maxEntries`);
  if (cache.staleMs !== undefined && (cache.staleMs as number) <= (cache.freshMs as number))
    unsupported(
      'staleMs must exceed freshMs; omit it to never serve a stale answer',
      context,
      `${field}.staleMs`,
      'client_contract.invalid_resilience',
    );
  if (cache.keyFields !== undefined) {
    const keyFields = strings(cache.keyFields, context, `${field}.keyFields`);
    if (keyFields.length === 0)
      unsupported(
        'keyFields must name at least one field; omit it to key on the whole request',
        context,
        `${field}.keyFields`,
        'client_contract.invalid_resilience',
      );
    unique(keyFields, context, `${field}.keyFields`);
    for (const [index, keyField] of keyFields.entries())
      if (!parseCacheKeyField(keyField))
        unsupported(
          `key field ${JSON.stringify(keyField)} must be body or <path|query|header|body>.<name>`,
          context,
          `${field}.keyFields.${index}`,
          'client_contract.invalid_resilience',
        );
  }
  if (cache.invalidationFields !== undefined) {
    const invalidationFields = strings(cache.invalidationFields, context, `${field}.invalidationFields`);
    if (invalidationFields.length === 0)
      unsupported(
        'invalidationFields must name at least one field; omit it to drop answers by key prefix only',
        context,
        `${field}.invalidationFields`,
        'client_contract.invalid_resilience',
      );
    unique(invalidationFields, context, `${field}.invalidationFields`);
    for (const [index, invalidationField] of invalidationFields.entries())
      if (!isCacheInvalidationField(invalidationField))
        unsupported(
          `invalidation field ${JSON.stringify(invalidationField)} must name a top-level response property`,
          context,
          `${field}.invalidationFields.${index}`,
          'client_contract.invalid_resilience',
        );
  }
}

/**
 * One declared invalidation field: a property name as it appears on the wire,
 * with no leading or trailing space and no control character — the byte rule
 * the Go reader applies (`ParseCacheInvalidationField`).
 */
function isCacheInvalidationField(value: string): boolean {
  return value !== '' && !value.startsWith(' ') && !value.endsWith(' ') && ![...value].some(isControlCharacter);
}

/** Split a declared key field into its section and name; the whole body is `['body', '']`. */
function parseCacheKeyField(value: string): [section: 'path' | 'query' | 'header' | 'body', name: string] | undefined {
  if (value === 'body') return ['body', ''];
  const dot = value.indexOf('.');
  if (dot < 0) return undefined;
  const section = value.slice(0, dot);
  const name = value.slice(dot + 1);
  if (!name || name.trim() !== name || [...name].some(isControlCharacter)) return undefined;
  if (section === 'header') return /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name) ? ['header', name] : undefined;
  if (section === 'path' || section === 'query' || section === 'body') return [section, name];
  return undefined;
}

function isControlCharacter(char: string): boolean {
  const code = char.codePointAt(0) ?? 0;
  return code < 0x20 || code === 0x7f;
}

/**
 * Refuse a key field that names no input the operation declares. It would key
 * every request on the same absent value and hand one caller another caller's
 * answer.
 */
function validateCacheKeyFields(
  client: ClientContractOperation,
  parameters: readonly ParameterIR[],
  request: RequestIR | undefined,
  doc: OpenApiDocument,
  context: OperationContext,
): void {
  const keyFields = client.resilience?.cache?.keyFields;
  if (!keyFields) return;
  const json = request?.content.find((content) => content.mediaType.toLowerCase() === 'application/json');
  const bodySchema = json?.schema?.$ref
    ? doc.components?.schemas?.[refName(json.schema.$ref)]
    : (json?.schema as Record<string, unknown> | undefined);
  const bodyProperties = Object.keys(
    (bodySchema as { properties?: Record<string, unknown> } | undefined)?.properties ?? {},
  );
  for (const [index, keyField] of keyFields.entries()) {
    const parsed = parseCacheKeyField(keyField);
    const [section, name] = parsed ?? ['', ''];
    const declared =
      section === 'body'
        ? request !== undefined && (name === '' || bodyProperties.includes(name))
        : parameters.some(
            (parameter) =>
              parameter.location === section &&
              (section === 'header' ? parameter.name.toLowerCase() === name.toLowerCase() : parameter.name === name),
          );
    if (!declared)
      unsupported(
        `key field ${JSON.stringify(keyField)} names no request input this operation declares`,
        context,
        `x-putnami-client.resilience.cache.keyFields.${index}`,
        'client_contract.invalid_resilience',
      );
  }
}

/**
 * Refuse a cursor continuation whose output field or query parameter is not a
 * plain string the operation declares (clientcontract ADR 0013): the runtime
 * copies a position verbatim from one to the other and never interprets it.
 * The rule is the Go reader's `ValidateSSEContinuationReferences`: one local
 * component reference is followed for the message and for each schema.
 */
function validateSseContinuationReferences(
  client: ClientContractOperation,
  parameters: readonly ParameterIR[],
  doc: OpenApiDocument,
  context: OperationContext,
): void {
  const components = (doc.components?.schemas ?? {}) as Record<string, unknown>;
  const resolve = (schema: unknown): Record<string, unknown> | undefined => {
    const node = schema as Record<string, unknown> | undefined;
    const ref = node?.['$ref'];
    if (typeof ref !== 'string') return node;
    return ref.startsWith('#/components/schemas/')
      ? (components[refName(ref)] as Record<string, unknown> | undefined)
      : undefined;
  };
  const output = resolve(client.messages?.output);
  for (const [index, transport] of client.transports.entries()) {
    const continuation = transport.protocol === 'sse' ? transport.sse?.continuation : undefined;
    if (continuation?.mode !== 'cursor' || continuation.cursor === undefined) continue;
    const field = `x-putnami-client.transports.${index}.sse.continuation.cursor`;
    const { outputField, queryParameter } = continuation.cursor;
    const properties =
      output?.['type'] === 'object' ? (output['properties'] as Record<string, unknown> | undefined) : undefined;
    const property = properties && Object.hasOwn(properties, outputField) ? properties[outputField] : undefined;
    const required = Array.isArray(output?.['required']) ? (output['required'] as unknown[]) : [];
    if (property === undefined)
      unsupported(
        `output field ${JSON.stringify(outputField)} names no property of the declared output message`,
        context,
        `${field}.outputField`,
        'client_contract.invalid_resilience',
      );
    if (!required.includes(outputField))
      unsupported(
        `output field ${JSON.stringify(outputField)} must be required: every message carries the position after it`,
        context,
        `${field}.outputField`,
        'client_contract.invalid_resilience',
      );
    if (!isCursorText(resolve(property)))
      unsupported(
        `output field ${JSON.stringify(outputField)} must be a plain string: a position is opaque text`,
        context,
        `${field}.outputField`,
        'client_contract.invalid_resilience',
      );
    const parameter = parameters.find((entry) => entry.location === 'query' && entry.name === queryParameter);
    if (parameter === undefined)
      unsupported(
        `query parameter ${JSON.stringify(queryParameter)} is not declared by this operation`,
        context,
        `${field}.queryParameter`,
        'client_contract.invalid_resilience',
      );
    if (!isCursorText(resolve(parameter.schema)))
      unsupported(
        `query parameter ${JSON.stringify(queryParameter)} must be a plain string: a position is opaque text`,
        context,
        `${field}.queryParameter`,
        'client_contract.invalid_resilience',
      );
  }
}

/**
 * A schema whose values a runtime can carry verbatim as a position: a string
 * with no format, no enum, no union and no null. Mirrors the Go reader's
 * `sseCursorText`.
 */
function isCursorText(schema: Record<string, unknown> | undefined): boolean {
  if (schema === undefined) return false;
  const enumValues = schema['enum'];
  const oneOf = schema['oneOf'];
  return (
    schema['$ref'] === undefined &&
    schema['type'] === 'string' &&
    !schema['format'] &&
    schema['nullable'] !== true &&
    !(Array.isArray(enumValues) && enumValues.length > 0) &&
    !(Array.isArray(oneOf) && oneOf.length > 0) &&
    schema['x-putnami-json'] === undefined
  );
}

/**
 * Refuse an invalidation field that names no comparable property of the
 * operation's JSON success body: it would tag no answer, and every
 * invalidation by it would silently drop nothing.
 */
function validateCacheInvalidationFields(
  client: ClientContractOperation,
  successes: readonly SuccessIR[],
  doc: OpenApiDocument,
  context: OperationContext,
): void {
  const fields = client.resilience?.cache?.invalidationFields;
  if (!fields) return;
  const properties = cacheResponseProperties(successes, (doc.components?.schemas ?? {}) as Record<string, unknown>);
  for (const [index, field] of fields.entries()) {
    const scalar = properties.get(field);
    if (scalar === true) continue;
    unsupported(
      scalar === undefined
        ? `invalidation field ${JSON.stringify(field)} names no top-level property of a JSON success body this operation declares`
        : `invalidation field ${JSON.stringify(field)} must name a string, integer or boolean property; the runtimes compare no other value`,
      context,
      `x-putnami-client.resilience.cache.invalidationFields.${index}`,
      'client_contract.invalid_resilience',
    );
  }
}

/**
 * The top-level properties of an operation's JSON success bodies, each mapped
 * to whether every body that declares it declares a comparable value. The rule
 * is the Go reader's (`CacheResponseProperties` in protocols/clientcontract,
 * ADR 0007): one local component reference is followed for a body and for each
 * property; a string, an integer or a boolean is comparable, but a string
 * declared `format: byte` or `binary` is not — this runtime decodes it to a byte
 * array no invalidation value can equal. The shared vectors in
 * protocols/clientcontract/fixtures/cache/invalidation.json pin the rule.
 */
export function cacheResponseProperties(
  successes: readonly SuccessIR[],
  components: Readonly<Record<string, unknown>>,
): Map<string, boolean> {
  const resolve = (schema: unknown): Record<string, unknown> | undefined => {
    const node = schema as Record<string, unknown> | undefined;
    const ref = node?.['$ref'];
    if (typeof ref !== 'string') return node;
    return ref.startsWith('#/components/schemas/')
      ? (components[refName(ref)] as Record<string, unknown> | undefined)
      : undefined;
  };
  const properties = new Map<string, boolean>();
  for (const success of successes) {
    for (const content of success.content) {
      if (content.mediaType.toLowerCase() !== 'application/json') continue;
      const body = resolve(content.schema);
      const declared = (body?.['properties'] ?? {}) as Record<string, unknown>;
      for (const [name, property] of Object.entries(declared))
        properties.set(name, isCacheComparable(resolve(property)) && (properties.get(name) ?? true));
    }
  }
  return properties;
}

function isCacheComparable(schema: Record<string, unknown> | undefined): boolean {
  if (schema === undefined || schema['$ref'] !== undefined || schema['x-putnami-json'] !== undefined) return false;
  const oneOf = schema['oneOf'];
  if (Array.isArray(oneOf) && oneOf.length > 0) return false;
  const type = schema['type'];
  if (type === 'string') return schema['format'] !== 'byte' && schema['format'] !== 'binary';
  return type === 'integer' || type === 'boolean';
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one closed pass validates descriptor cross-references
function validateProtobuf(input: unknown): void {
  const field = 'x-putnami-client.protobuf';
  const descriptor = object(input, undefined, field);
  assertKnownKeys(descriptor, ['syntax', 'package', 'services', 'messages', 'enums'], undefined, field);
  if (descriptor.syntax !== 'proto3') unsupported('only proto3 is supported', undefined, `${field}.syntax`);
  string(descriptor.package, undefined, `${field}.package`);
  if (!Array.isArray(descriptor.services) || !Array.isArray(descriptor.messages) || !Array.isArray(descriptor.enums))
    unsupported('services, messages, and enums must be arrays', undefined, field);
  const messages = new Set<string>();
  const enums = new Set<string>();
  const messageEntries: { message: StrictObject; field: string }[] = [];
  for (const [index, entry] of descriptor.messages.entries()) {
    const messageField = `${field}.messages.${index}`;
    const message = object(entry, undefined, messageField);
    assertKnownKeys(message, ['name', 'fields', 'oneofs'], undefined, messageField);
    const name = string(message.name, undefined, `${messageField}.name`);
    if (messages.has(name)) unsupported('duplicate protobuf message', undefined, `${messageField}.name`);
    messages.add(name);
    if (!Array.isArray(message.fields)) unsupported('fields must be an array', undefined, `${messageField}.fields`);
    if (message.oneofs !== undefined)
      unique(strings(message.oneofs, undefined, `${messageField}.oneofs`), undefined, `${messageField}.oneofs`);
    messageEntries.push({ message, field: messageField });
  }
  for (const [index, entry] of descriptor.enums.entries()) {
    const enumField = `${field}.enums.${index}`;
    const enumType = object(entry, undefined, enumField);
    assertKnownKeys(enumType, ['name', 'values'], undefined, enumField);
    const name = string(enumType.name, undefined, `${enumField}.name`);
    if (enums.has(name)) unsupported('duplicate protobuf enum', undefined, `${enumField}.name`);
    enums.add(name);
    if (!Array.isArray(enumType.values) || !enumType.values.length)
      unsupported('enum values must not be empty', undefined, `${enumField}.values`);
    const numbers = new Set<number>();
    const names = new Set<string>();
    enumType.values.forEach((valueEntry, valueIndex) => {
      const valueField = `${enumField}.values.${valueIndex}`;
      const value = object(valueEntry, undefined, valueField);
      assertKnownKeys(value, ['name', 'number'], undefined, valueField);
      const valueName = string(value.name, undefined, `${valueField}.name`);
      if (!Number.isInteger(value.number))
        unsupported('enum number must be integer', undefined, `${valueField}.number`);
      if (numbers.has(value.number as number) || names.has(valueName))
        unsupported('duplicate enum name or number', undefined, valueField);
      numbers.add(value.number as number);
      names.add(valueName);
    });
  }
  for (const entry of messageEntries) {
    validateProtobufFields(
      entry.message.fields as unknown[],
      new Set(entry.message.oneofs as string[] | undefined),
      messages,
      enums,
      entry.field,
    );
  }
  const services = new Set<string>();
  const methods = new Set<string>();
  for (const [index, entry] of descriptor.services.entries()) {
    const serviceField = `${field}.services.${index}`;
    const service = object(entry, undefined, serviceField);
    assertKnownKeys(service, ['name', 'methods'], undefined, serviceField);
    const serviceName = string(service.name, undefined, `${serviceField}.name`);
    if (services.has(serviceName)) unsupported('duplicate protobuf service', undefined, `${serviceField}.name`);
    services.add(serviceName);
    if (!Array.isArray(service.methods)) unsupported('methods must be an array', undefined, `${serviceField}.methods`);
    const serviceMethods = new Set<string>();
    service.methods.forEach((methodEntry, methodIndex) => {
      const methodField = `${serviceField}.methods.${methodIndex}`;
      const method = object(methodEntry, undefined, methodField);
      assertKnownKeys(
        method,
        ['name', 'input', 'output', 'clientStreaming', 'serverStreaming'],
        undefined,
        methodField,
      );
      const methodName = string(method.name, undefined, `${methodField}.name`);
      const input = string(method.input, undefined, `${methodField}.input`);
      const output = string(method.output, undefined, `${methodField}.output`);
      if (!isKnownProtobufMessage(input, messages))
        unsupported('protobuf input message is not declared', undefined, `${methodField}.input`);
      if (!isKnownProtobufMessage(output, messages))
        unsupported('protobuf output message is not declared', undefined, `${methodField}.output`);
      boolean(method.clientStreaming, undefined, `${methodField}.clientStreaming`);
      boolean(method.serverStreaming, undefined, `${methodField}.serverStreaming`);
      if (serviceMethods.has(methodName)) unsupported('duplicate protobuf method', undefined, `${methodField}.name`);
      serviceMethods.add(methodName);
      const key = `${serviceName}/${methodName}`;
      if (methods.has(key)) unsupported('duplicate protobuf method', undefined, methodField);
      methods.add(key);
    });
  }
}

const PROTOBUF_SCALARS = new Set([
  'double',
  'float',
  'int64',
  'uint64',
  'int32',
  'fixed64',
  'fixed32',
  'bool',
  'string',
  'bytes',
  'uint32',
  'sfixed32',
  'sfixed64',
  'sint32',
  'sint64',
]);
const PROTOBUF_MAP_KEYS = new Set([
  'int32',
  'int64',
  'uint32',
  'uint64',
  'sint32',
  'sint64',
  'fixed32',
  'fixed64',
  'sfixed32',
  'sfixed64',
  'bool',
  'string',
]);
const PROTOBUF_WELL_KNOWN_MESSAGES = new Set([
  'google.protobuf.Any',
  'google.protobuf.Duration',
  'google.protobuf.Empty',
  'google.protobuf.Timestamp',
]);

function isKnownProtobufMessage(name: string, messages: Set<string>): boolean {
  return messages.has(name) || PROTOBUF_WELL_KNOWN_MESSAGES.has(name);
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: field-kind checks deliberately fail at the exact field
function validateProtobufFields(
  input: unknown[],
  oneofs: Set<string>,
  messages: Set<string>,
  enums: Set<string>,
  parentField: string,
): void {
  const numbers = new Set<number>();
  const names = new Set<string>();
  const jsonNames = new Set<string>();
  let lastNumber = 0;
  for (const [index, entry] of input.entries()) {
    const field = `${parentField}.fields.${index}`;
    const protoField = object(entry, undefined, field);
    assertKnownKeys(
      protoField,
      ['name', 'jsonName', 'number', 'typeKind', 'type', 'repeated', 'optional', 'oneof', 'map'],
      undefined,
      field,
    );
    const name = string(protoField.name, undefined, `${field}.name`);
    const jsonName = string(protoField.jsonName, undefined, `${field}.jsonName`);
    if (
      !Number.isInteger(protoField.number) ||
      (protoField.number as number) < 1 ||
      (protoField.number as number) > 536_870_911 ||
      ((protoField.number as number) >= 19_000 && (protoField.number as number) <= 19_999)
    )
      unsupported('invalid protobuf field number', undefined, `${field}.number`);
    const number = protoField.number as number;
    if (number <= lastNumber)
      unsupported('protobuf fields must be in ascending field-number order', undefined, `${field}.number`);
    lastNumber = number;
    if (numbers.has(number) || names.has(name) || jsonNames.has(jsonName))
      unsupported('duplicate protobuf field number, name, or jsonName', undefined, field);
    numbers.add(number);
    names.add(name);
    jsonNames.add(jsonName);
    if (!['scalar', 'message', 'enum', 'map'].includes(protoField.typeKind as string))
      unsupported('unsupported protobuf field kind', undefined, `${field}.typeKind`);
    const type = string(protoField.type, undefined, `${field}.type`);
    if (protoField.repeated !== undefined) boolean(protoField.repeated, undefined, `${field}.repeated`);
    if (protoField.optional !== undefined) boolean(protoField.optional, undefined, `${field}.optional`);
    if (protoField.oneof !== undefined && !oneofs.has(string(protoField.oneof, undefined, `${field}.oneof`)))
      unsupported('field references unknown oneof', undefined, `${field}.oneof`);
    if (protoField.typeKind === 'scalar' && !PROTOBUF_SCALARS.has(type))
      unsupported('unsupported protobuf scalar type', undefined, `${field}.type`);
    if (protoField.typeKind === 'message' && !isKnownProtobufMessage(type, messages))
      unsupported('protobuf message type is not declared', undefined, `${field}.type`);
    if (protoField.typeKind === 'enum' && !enums.has(type))
      unsupported('protobuf enum type is not declared', undefined, `${field}.type`);
    if (protoField.typeKind === 'map') {
      const map = object(protoField.map, undefined, `${field}.map`);
      assertKnownKeys(map, ['keyType', 'valueKind', 'valueType'], undefined, `${field}.map`);
      const keyType = string(map.keyType, undefined, `${field}.map.keyType`);
      if (!PROTOBUF_MAP_KEYS.has(keyType))
        unsupported('unsupported protobuf map key type', undefined, `${field}.map.keyType`);
      if (!['scalar', 'message', 'enum'].includes(map.valueKind as string))
        unsupported('invalid map value kind', undefined, `${field}.map.valueKind`);
      const valueType = string(map.valueType, undefined, `${field}.map.valueType`);
      if (map.valueKind === 'scalar' && !PROTOBUF_SCALARS.has(valueType))
        unsupported('unsupported protobuf map scalar type', undefined, `${field}.map.valueType`);
      if (map.valueKind === 'message' && !isKnownProtobufMessage(valueType, messages))
        unsupported('protobuf map message type is not declared', undefined, `${field}.map.valueType`);
      if (map.valueKind === 'enum' && !enums.has(valueType))
        unsupported('protobuf map enum type is not declared', undefined, `${field}.map.valueType`);
    } else if (protoField.map !== undefined)
      unsupported('map metadata requires map typeKind', undefined, `${field}.map`);
    if (
      protoField.repeated === true &&
      (protoField.optional === true || protoField.oneof !== undefined || protoField.typeKind === 'map')
    )
      unsupported('repeated cannot be combined with optional, oneof, or map', undefined, field);
    if (protoField.optional === true && protoField.oneof !== undefined)
      unsupported('optional cannot be combined with oneof', undefined, field);
  }
}

function validateProtobufMethodReference(
  reference: string,
  descriptor: ClientProtobufDescriptor,
  stream: ClientContractOperation['stream'],
  context: OperationContext,
  field: string,
): void {
  const match = reference.match(/^\/([A-Za-z_][A-Za-z0-9_.]*)\/([A-Za-z_][A-Za-z0-9_]*)$/);
  if (!match?.[1].startsWith(`${descriptor.package}.`))
    unsupported('invalid protobuf method reference', context, field, 'client_contract.invalid_protobuf');
  const serviceName = match[1].slice(descriptor.package.length + 1);
  const method = descriptor.services
    .find((service) => service.name === serviceName)
    ?.methods.find((candidate) => candidate.name === match[2]);
  if (!method)
    unsupported('protobuf method is absent from descriptor', context, field, 'client_contract.invalid_protobuf');
  const clientStreaming = stream === 'client' || stream === 'bidirectional';
  const serverStreaming = stream === 'server' || stream === 'bidirectional';
  if (method.clientStreaming !== clientStreaming || method.serverStreaming !== serverStreaming)
    unsupported('protobuf streaming flags disagree with operation', context, field, 'client_contract.invalid_protobuf');
}

function validateTransportPath(value: string, context: OperationContext, field: string): void {
  if (!value.startsWith('/') || value.startsWith('//') || /[?#\\\r\n]/.test(value) || value.includes('//'))
    unsupported('unsafe same-authority transport path', context, field, 'client_contract.invalid_transport');
  let decoded: string;
  try {
    decoded = decodeURIComponent(value);
  } catch {
    unsupported('invalid path encoding', context, field, 'client_contract.invalid_transport');
  }
  if (
    decoded.includes('\\') ||
    decoded.includes('//') ||
    decoded.split('/').some((segment) => segment === '.' || segment === '..')
  )
    unsupported('unsafe decoded transport path', context, field, 'client_contract.invalid_transport');
}

function validateJsonNumber(value: unknown, context: OperationContext | undefined, field: string): void {
  if (isExactJsonNumber(value)) return;
  if (typeof value !== 'number' || !Number.isFinite(value))
    unsupported('expected a finite JSON number', context, field);
  if (Number.isInteger(value) && !Number.isSafeInteger(value)) {
    unsupported('unsafe integer lost precision; read first-party contracts from raw source', context, field);
  }
}

function validateJsonValue(value: unknown, context: OperationContext | undefined, field: string): void {
  if (isExactJsonNumber(value)) return;
  if (typeof value === 'number') {
    validateJsonNumber(value, context, field);
    return;
  }
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return;
  if (Array.isArray(value)) {
    value.forEach((entry, index) => {
      validateJsonValue(entry, context, `${field}.${index}`);
    });
    return;
  }
  if (value && typeof value === 'object') {
    for (const [key, entry] of Object.entries(value)) validateJsonValue(entry, context, `${field}.${key}`);
    return;
  }
  unsupported('default must be a JSON value', context, field);
}

function cloneContractValue(value: unknown): unknown {
  if (isExactJsonNumber(value)) return value;
  if (Array.isArray(value)) return value.map(cloneContractValue);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, cloneContractValue(entry)]));
  }
  return value;
}

function serializeCanonicalJson(value: unknown): string {
  if (isExactJsonNumber(value)) return value.$number;
  if (value === null) return 'null';
  if (typeof value === 'string' || typeof value === 'boolean' || typeof value === 'number') {
    const encoded = JSON.stringify(value);
    if (encoded === undefined) throw new TypeError('client IR contains a non-JSON value');
    return encoded;
  }
  if (Array.isArray(value)) return `[${value.map(serializeCanonicalJson).join(',')}]`;
  if (value && typeof value === 'object') {
    const entries = Object.entries(value)
      .filter(([, entry]) => entry !== undefined)
      .sort(([left], [right]) => (left < right ? -1 : left > right ? 1 : 0));
    return `{${entries.map(([key, entry]) => `${JSON.stringify(key)}:${serializeCanonicalJson(entry)}`).join(',')}}`;
  }
  throw new TypeError('client IR contains a non-JSON value');
}

class StrictJsonParser {
  private offset = 0;

  constructor(private readonly source: string) {}

  parse(): unknown {
    const result = this.value('$');
    this.whitespace();
    if (this.offset !== this.source.length) this.fail('$', 'trailing JSON content');
    return result;
  }

  private value(field: string): unknown {
    this.whitespace();
    const character = this.source[this.offset];
    if (character === '{') return this.object(field);
    if (character === '[') return this.array(field);
    if (character === '"') return this.string(field);
    if (character === '-' || (character >= '0' && character <= '9')) return this.number(field);
    if (this.source.startsWith('true', this.offset)) {
      this.offset += 4;
      return true;
    }
    if (this.source.startsWith('false', this.offset)) {
      this.offset += 5;
      return false;
    }
    if (this.source.startsWith('null', this.offset)) {
      this.offset += 4;
      return null;
    }
    return this.fail(field, 'invalid JSON value');
  }

  private object(field: string): Record<string, unknown> {
    this.offset++;
    this.whitespace();
    const result: Record<string, unknown> = {};
    const seen = new Set<string>();
    if (this.take('}')) return result;
    while (true) {
      const key = this.string(field);
      if (seen.has(key)) this.fail(`${field}.${key}`, 'duplicate JSON object key');
      seen.add(key);
      this.whitespace();
      if (!this.take(':')) this.fail(field, 'expected colon after object key');
      result[key] = this.value(`${field}.${key}`);
      this.whitespace();
      if (this.take('}')) return result;
      if (!this.take(',')) this.fail(field, 'expected comma between object entries');
      this.whitespace();
    }
  }

  private array(field: string): unknown[] {
    this.offset++;
    this.whitespace();
    const result: unknown[] = [];
    if (this.take(']')) return result;
    while (true) {
      result.push(this.value(`${field}.${result.length}`));
      this.whitespace();
      if (this.take(']')) return result;
      if (!this.take(',')) this.fail(field, 'expected comma between array entries');
    }
  }

  private string(field: string): string {
    this.whitespace();
    if (this.source[this.offset] !== '"') return this.fail(field, 'expected JSON string');
    const start = this.offset++;
    let escaped = false;
    while (this.offset < this.source.length) {
      const character = this.source[this.offset++];
      if (escaped) {
        escaped = false;
        continue;
      }
      if (character === '\\') {
        escaped = true;
        continue;
      }
      if (character === '"') {
        try {
          return JSON.parse(this.source.slice(start, this.offset)) as string;
        } catch {
          return this.fail(field, 'invalid JSON string');
        }
      }
      if (character < ' ') return this.fail(field, 'unescaped control character in string');
    }
    return this.fail(field, 'unterminated JSON string');
  }

  private number(field: string): number | ExactJsonNumber {
    const match = this.source.slice(this.offset).match(/^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/);
    if (!match) return this.fail(field, 'invalid JSON number');
    const token = match[0];
    this.offset += token.length;
    const value = Number(token);
    if (!Number.isFinite(value)) return this.fail(field, 'JSON number is not finite');
    if (!/[.eE]/.test(token) && !Number.isSafeInteger(value)) return new ExactJsonNumber(token);
    return value;
  }

  private take(character: string): boolean {
    if (this.source[this.offset] !== character) return false;
    this.offset++;
    return true;
  }

  private whitespace(): void {
    while (/\s/.test(this.source[this.offset] ?? '')) this.offset++;
  }

  private fail(field: string, message: string): never {
    throw new ClientGenerationError('clientgen_unsupported_semantic', message, {
      field: `${field} (byte ${this.offset})`,
    });
  }
}

function validateHeader(
  value: string,
  context: OperationContext | undefined,
  field: string,
  credential: boolean,
  contractCode: ClientContractDiagnosticCode = 'client_contract.invalid_credential',
): void {
  if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(value)) unsupported('invalid header name', context, field, contractCode);
  if (credential && isFrameworkReservedHeader(value))
    unsupported('credential header is reserved', context, field, contractCode);
}

function isFrameworkReservedHeader(value: string): boolean {
  return new Set([
    'authorization',
    'host',
    'content-length',
    'connection',
    'transfer-encoding',
    'upgrade',
    'cookie',
    'set-cookie',
    'traceparent',
    'tracestate',
    'baggage',
    'x-client-id',
    'x-putnami-service',
    // The SSE wire negotiation marker (clientcontract ADR 0013).
    'x-putnami-stream-wire',
  ]).has(value.toLowerCase());
}

/**
 * A budget field. A value of the wrong JSON type never reached the declared
 * shape, so it is a parse defect; a well-typed value outside its allowed range
 * is a defect of the section that declares it.
 */
function positive(
  value: unknown,
  context: OperationContext | undefined,
  field: string,
  contractCode: ClientContractDiagnosticCode = 'client_contract.invalid_resilience',
): void {
  if (value === undefined) return;
  if (typeof value !== 'number') unsupported('expected a positive safe integer', context, field);
  if (!Number.isSafeInteger(value) || value <= 0)
    unsupported('expected a positive safe integer', context, field, contractCode);
}

function object(input: unknown, context: OperationContext | undefined, field: string): StrictObject {
  if (!input || typeof input !== 'object' || Array.isArray(input)) unsupported('expected an object', context, field);
  return input as StrictObject;
}

function assertKnownKeys(
  input: Record<string, unknown>,
  allowed: readonly string[],
  context: OperationContext | undefined,
  field: string,
): void {
  const known = new Set(allowed);
  for (const key of Object.keys(input))
    if (!known.has(key))
      unsupported(`unknown field ${JSON.stringify(key)}`, context, `${field}.${key}`, 'client_contract.unknown_field');
}

function string(input: unknown, context: OperationContext | undefined, field: string, allowEmpty = false): string {
  if (typeof input !== 'string' || (!allowEmpty && !input.trim()))
    unsupported('expected a non-empty string', context, field);
  return input as string;
}

function boolean(input: unknown, context: OperationContext | undefined, field: string): boolean {
  if (typeof input !== 'boolean') unsupported('expected a boolean', context, field);
  return input as boolean;
}

function strings(input: unknown, context: OperationContext | undefined, field: string): string[] {
  if (!Array.isArray(input)) unsupported('expected a string array', context, field);
  return input.map((entry, index) => string(entry, context, `${field}.${index}`));
}

function unique(values: readonly string[], context: OperationContext | undefined, field: string): void {
  if (new Set(values).size !== values.length)
    unsupported('duplicate values', context, field, 'client_contract.duplicate');
}

/**
 * Refuse a document. `contractCode` names the defect with the same vocabulary the
 * Go reader uses; it defaults to a parse defect, which is what a value of the
 * wrong JSON type or shape is on both sides.
 */
function unsupported(
  message: string,
  context: OperationContext | undefined,
  field: string,
  contractCode: ClientContractDiagnosticCode = 'client_contract.parse_error',
): never {
  throw new ClientGenerationError('clientgen_unsupported_semantic', message, { ...context, field, contractCode });
}

function readThirdPartyOpenApiSpec(doc: OpenApiDocument): SpecIR {
  const serviceMap = new Map<string, MethodIR[]>();
  for (const [path, pathItem] of Object.entries(doc.paths)) {
    for (const [httpMethod, operation] of Object.entries(pathItem)) {
      if (!HTTP_OPERATION_KEYS.has(httpMethod.toLowerCase())) continue;
      const method = legacyOperationToMethod(path, httpMethod.toUpperCase(), operation);
      const serviceName = inferServiceName(path);
      const methods = serviceMap.get(serviceName) ?? [];
      methods.push(method);
      serviceMap.set(serviceName, methods);
    }
  }
  const services = [...serviceMap].map(
    ([name, methods]): ServiceIR => ({ name, className: name.replace(/Service$/, 'Client'), methods }),
  );
  const { namedTypes, enums, unions } = buildComponentTypes(doc);
  return {
    irVersion: CLIENT_IR_VERSION,
    transport: 'http',
    services,
    ...(namedTypes ? { namedTypes } : {}),
    ...(enums ? { enums } : {}),
    ...(unions ? { unions } : {}),
    specHash: computeSpecHash(JSON.stringify(doc)),
  };
}

function inferServiceName(path: string): string {
  const first =
    path
      .split('/')
      .filter(Boolean)
      .find((segment) => !segment.startsWith('{')) ?? 'api';
  // A path segment may carry characters no identifier can (`/.well-known/...`).
  // Every one of them separates words, not only `-` and `_`, so the class name
  // is a legal identifier. go/framework/api clientir.go applies the same rule.
  const name = pascalCase(first.replace(/[^A-Za-z0-9]+/g, '-')) || 'Api';
  return `${/^[0-9]/.test(name) ? `_${name}` : name}Service`;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: compatibility projection retains the legacy mapping
function legacyOperationToMethod(
  path: string,
  httpMethod: string,
  operation: OpenApiDocument['paths'][string][string],
): MethodIR {
  const operationId = operation.operationId ?? buildOperationId(httpMethod, path);
  const method: MethodIR = { name: toSafeIdentifier(toCamelCase(operationId)), operationId, httpMethod, path };
  if (operation.parameters) {
    const params: FieldIR[] = [];
    const query: FieldIR[] = [];
    for (const parameter of operation.parameters) {
      const field = parameterToField(parameter);
      if (parameter.in === 'path') params.push(field);
      else if (parameter.in === 'query') query.push(field);
    }
    if (params.length) method.params = params;
    if (query.length) method.query = query;
  }
  const bodySchema = operation.requestBody?.content?.['application/json']?.schema;
  if (bodySchema?.$ref) method.bodyType = refName(bodySchema.$ref);
  else if (bodySchema) {
    const fields = schemaToFields(bodySchema);
    if (fields.length) method.body = fields;
  }
  const responseSchema = pickResponseSchema(operation.responses);
  if (responseSchema?.$ref) method.responseType = refName(responseSchema.$ref);
  else if (responseSchema) {
    const fields = schemaToFields(responseSchema);
    if (fields.length) method.response = fields;
  }
  return method;
}

function pickResponseSchema(responses: Record<string, unknown> | undefined): OpenApiSchema | undefined {
  if (!responses) return undefined;
  const codes = Object.keys(responses).filter((code) => /^2\d\d$/.test(code));
  const ordered = [
    ...codes.filter((code) => code === '200'),
    ...codes.filter((code) => code === '201'),
    ...codes.filter((code) => code !== '200' && code !== '201').sort(),
  ];
  for (const code of ordered) {
    const response = responses[code] as { content?: Record<string, { schema?: OpenApiSchema }> } | undefined;
    const schema = response?.content?.['application/json']?.schema;
    if (schema) return schema;
  }
  return undefined;
}

function parameterToField(parameter: { name: string; required: boolean; schema: OpenApiSchema }): FieldIR {
  const { tsType, array } = resolveFieldType(parameter.schema ?? {});
  return { name: parameter.name, tsType, optional: !parameter.required, array };
}

function buildComponentTypes(doc: OpenApiDocument): {
  namedTypes?: Record<string, FieldIR[]>;
  enums?: Record<string, string[]>;
  unions?: Record<string, UnionIR>;
} {
  const schemas = doc.components?.schemas;
  if (!schemas || !Object.keys(schemas).length) return {};
  const namedTypes: Record<string, FieldIR[]> = {};
  const enums: Record<string, string[]> = {};
  const unions: Record<string, UnionIR> = {};
  for (const [name, schema] of Object.entries(schemas)) {
    if (Array.isArray(schema.enum) && schema.enum.every((entry) => typeof entry === 'string'))
      enums[name] = schema.enum as string[];
    else if (Array.isArray(schema.oneOf) && schema.discriminator?.propertyName)
      unions[name] = schemaToUnion(schema, doc);
    else namedTypes[name] = schemaToFields(schema);
  }
  return {
    ...(Object.keys(namedTypes).length ? { namedTypes } : {}),
    ...(Object.keys(enums).length ? { enums } : {}),
    ...(Object.keys(unions).length ? { unions } : {}),
  };
}

function schemaToUnion(schema: OpenApiSchema, doc: OpenApiDocument): UnionIR {
  const discriminator = schema.discriminator?.propertyName ?? '';
  return {
    discriminator,
    variants: (schema.oneOf ?? []).map((arm) => {
      const resolved = dereferenceSchema(arm, doc);
      return {
        tag: String(resolved.properties?.[discriminator]?.enum?.[0] ?? mappedDiscriminatorTag(schema, arm.$ref) ?? ''),
        fields: schemaToFields(resolved).filter((field) => field.name !== discriminator),
      };
    }),
  };
}

function dereferenceSchema(schema: OpenApiSchema, doc: OpenApiDocument): OpenApiSchema {
  return schema.$ref ? (doc.components?.schemas?.[refName(schema.$ref)] ?? schema) : schema;
}

function mappedDiscriminatorTag(schema: OpenApiSchema, armRef: string | undefined): string | undefined {
  if (!armRef) return undefined;
  return Object.entries(schema.discriminator?.mapping ?? {}).find(([, ref]) => refName(ref) === refName(armRef))?.[0];
}

function schemaToFields(schema: OpenApiSchema): FieldIR[] {
  if (!schema.properties) return [];
  const required = new Set(schema.required ?? []);
  return Object.entries(schema.properties)
    .filter(([name]) => !['__proto__', 'constructor', 'prototype'].includes(name))
    .map(([name, property]) => ({ name, ...resolveFieldType(property), optional: !required.has(name) }));
}

function resolveFieldType(schema: OpenApiSchema): { tsType: string; array: boolean } {
  if (schema.$ref) return { tsType: refName(schema.$ref), array: false };
  if (schema.type === 'array') {
    const items = schema.items ?? {};
    if (items.$ref) return { tsType: refName(items.$ref), array: true };
    if (items.type === 'object') return { tsType: 'Record<string, unknown>', array: true };
    return { tsType: openApiTypeToTs(items.type), array: true };
  }
  if (schema.type === 'object') return { tsType: 'Record<string, unknown>', array: false };
  return { tsType: openApiTypeToTs(schema.type), array: false };
}

function refName(ref: string): string {
  return ref.split('/').pop() ?? ref;
}

function openApiTypeToTs(type?: string): string {
  if (type === 'number' || type === 'integer') return 'number';
  if (type === 'boolean') return 'boolean';
  return 'string';
}

function buildOperationId(method: string, path: string): string {
  const segments = path
    .split('/')
    .filter(Boolean)
    .map((segment) => {
      const parameter = segment.match(/^\{(.+)}$/);
      return parameter ? `_${parameter[1]}` : `${segment.charAt(0).toUpperCase()}${segment.slice(1)}`;
    });
  return `${method.toLowerCase()}${segments.join('')}`;
}
