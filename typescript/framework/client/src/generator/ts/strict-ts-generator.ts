import {
  type ClientSchema,
  type ClientTransportContract,
  protoJsonName,
  toSnakeCase as toProtoSnakeCase,
} from '@putnami/application';
import type { ContentIR, MethodIR, ParameterIR, ServiceIR, SpecIR, SuccessIR } from '../ir.type';
import { assertSafeIdentifier, operationTypeBase, pascalCase, toKebabCase } from '../string-utils';
import type { GeneratedFile, TsGeneratorOptions } from './ts-generator';

/** Closed set of integer widths a contract may declare (D0.2). */
const INTEGER_FORMATS = new Set(['int32', 'int64', 'uint32', 'uint64']);

/** Base-client members a provider operation cannot shadow. */
const RESERVED_CLIENT_METHODS = new Set([
  'forEndpoint',
  'failedEndpointStream',
  'endpointBinding',
  'endpointClients',
  'baseUrl',
]);

/** First-party REST/SSE emitter. Unsupported transports fail before any file is returned. */
export function generateStrictTypeScriptClient(spec: SpecIR, options: TsGeneratorOptions): GeneratedFile[] {
  if (!spec.contract) throw new Error('clientgen_first_party_required: strict emitter requires a contract');
  validateStrictRestSpec(spec);
  const files: GeneratedFile[] = [
    { path: 'src/types.ts', content: generateStrictTypes(spec) },
    ...spec.services.map((service) => ({
      path: `src/${toKebabCase(service.className)}.ts`,
      content: generateStrictService(service, spec),
    })),
    { path: 'src/index.ts', content: generateStrictIndex(spec) },
    { path: 'package.json', content: generateStrictPackage(options) },
    {
      path: 'tsconfig.json',
      content: `${JSON.stringify({ extends: options.tsconfigExtends ?? '../../../../tsconfig.base.json', compilerOptions: {} }, null, 2)}\n`,
    },
  ];
  return files;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: strict projection validates every semantic branch before emitting files
function validateStrictRestSpec(spec: SpecIR): void {
  const generatedSymbols = new Map<string, string>();
  claimGeneratedSymbol(generatedSymbols, 'ClientCallOptions', 'framework call options');
  const hasErrors = spec.services.some((service) => service.methods.some((method) => method.client?.errors.length));
  const hasErrorSchemas = spec.services.some((service) =>
    service.methods.some((method) => method.client?.errors.some((error) => error.schema !== undefined)),
  );
  if (hasErrors) claimGeneratedSymbol(generatedSymbols, 'isClientFrameworkError', 'framework error guard import');
  if (hasErrorSchemas)
    claimGeneratedSymbol(generatedSymbols, 'CLIENT_ERROR_SCHEMAS', 'framework error schema registry');
  for (const [name, schema] of Object.entries(spec.schemas ?? {})) {
    assertSafeIdentifier(name, 'first-party schema name');
    claimGeneratedSymbol(generatedSymbols, name, `component schema ${name}`);
    schemaType(schema, spec, `components.schemas.${name}`);
  }
  for (const service of spec.services) {
    assertSafeIdentifier(service.className, 'first-party client class');
    claimGeneratedSymbol(generatedSymbols, service.className, `service class ${service.name}`);
    claimGeneratedSymbol(generatedSymbols, `register${service.className}`, `service binding ${service.name}`);
    claimGeneratedSymbol(generatedSymbols, `bind${service.className}`, `caller binding ${service.name}`);
    for (const method of service.methods) {
      assertSafeIdentifier(method.name, `operation ${method.operationId}`);
      if (RESERVED_CLIENT_METHODS.has(method.name)) {
        throw unsupported(method, 'method name conflicts with endpoint selection');
      }
      const operation = method.client;
      const transports = operation?.transports.map((entry) => entry.protocol) ?? [];
      const stream = operation?.stream;
      // D0.8: the wire v1 contract carries `encoding: "proto"`, but no proto codec
      // exists yet. Refuse at generation, naming the operation and the transport,
      // rather than emitting a client that fails on its first frame. Checked over
      // every declared transport, so a supported sibling cannot mask it.
      for (const [index, transport] of (operation?.transports ?? []).entries()) {
        if (transport.protocol === 'websocket' && transport.encoding === 'proto') {
          throw unsupported(
            method,
            `transports[${index}] websocket encoding "proto", which no first-party codec implements yet`,
          );
        }
      }
      // A body on a request the framework declares safe has no interoperable
      // meaning: intermediaries may drop it and the provider never reads it.
      if (['GET', 'HEAD'].includes(method.httpMethod.toUpperCase()) && method.request !== undefined) {
        throw unsupported(method, `a request body on ${method.httpMethod.toUpperCase()}`);
      }
      validateBinaryOperation(method);
      // Connect carries unary and server streams; it cannot carry a client or
      // bidirectional stream over HTTP/1.1, and the provider answers those
      // `unimplemented`.
      const connect = (operation?.transports ?? []).filter(
        (entry) => entry.protocol === 'connect' && (stream === 'unary' || stream === 'server'),
      );
      for (const entry of connect) validateConnectTransport(method, entry, spec);
      // A stream is carried by the first declared transport this runtime can
      // speak: Connect, SSE and WebSocket for a server stream, WebSocket alone
      // for the duplex modes. The declared order is the dispatch order, and the
      // emitted method signature is the same whichever transport wins, so the
      // choice never reaches the consuming application.
      const duplex = stream === 'client' || stream === 'bidirectional';
      // A provider-owned wire is the operation's only transport and has its own
      // entry point: raw octets carry no message schema, typed frames carry
      // both. The reader already refused every other combination.
      const wire = providerWireOf(method);
      const supported = wire
        ? stream === 'bidirectional' &&
          (wire.encoding === 'binary'
            ? operation?.messages === undefined
            : operation?.messages?.input !== undefined && operation?.messages?.output !== undefined)
        : (stream === 'unary' && (transports.includes('rest-json') || connect.length > 0)) ||
          (stream === 'server' &&
            (transports.includes('sse') || transports.includes('websocket') || connect.length > 0) &&
            operation?.messages?.output !== undefined) ||
          (duplex &&
            transports.includes('websocket') &&
            operation?.messages?.output !== undefined &&
            operation?.messages?.input !== undefined);
      if (!supported) {
        throw unsupported(method, `${stream ?? 'missing'} operation over ${transports.join(',') || 'no transport'}`);
      }
      // Connect narrows REST only where the call travels over it. The declared
      // order is the dispatch order, so a contract whose first carriable
      // transport is REST, SSE or WebSocket never reaches Connect and is not
      // refused for a Connect reason — the rule the Go emitter applies to the
      // same contract (go/framework/api clientgen_strict.go).
      const dispatched = (operation?.transports ?? []).find((entry) =>
        stream === 'unary'
          ? entry.protocol === 'rest-json' || entry.protocol === 'connect'
          : stream === 'server'
            ? entry.protocol === 'connect' || entry.protocol === 'sse' || entry.protocol === 'websocket'
            : entry.protocol === 'websocket',
      );
      if (dispatched?.protocol === 'connect') validateConnectSemantics(method, spec);
      // Resume continues a position the caller already consumed up to. Only a
      // server stream can do that: continuing a duplex conversation would
      // replay the caller's own messages. It changes no emitted line — the
      // runtime honors it from the declaration — so a server stream that
      // declares it emits exactly what one that does not emits.
      for (const transport of operation?.transports ?? []) {
        if (transport.protocol === 'websocket' && transport.websocket?.resume && stream !== 'server') {
          throw unsupported(method, 'websocket resume, which only a server stream can honor');
        }
      }
      // Reconnect asks the provider to continue a stream. A provider that
      // declares no continuable transport — a WebSocket with resume, or an
      // SSE transport with a continuation (ADR 0013) — cannot: continuing
      // would be re-opening, and re-opening a stream that already delivered
      // messages delivers them a second time.
      if (
        operation?.resilience?.stream?.reconnect &&
        !operation.transports.some((transport) => transport.websocket?.resume || transport.sse?.continuation)
      ) {
        throw unsupported(method, 'stream reconnect without a provider-declared resume transport');
      }
      for (const parameter of method.parameters ?? []) {
        // A parameter travels as text; an opaque JSON value has no text form
        // both runtimes would agree on. The check reads through component
        // references, so a referenced opaque value is refused exactly like an
        // inline one. The Go emitter refuses it the same way.
        const declared = resolveReference(parameter.schema, spec);
        const items = declared.type === 'array' && declared.items ? resolveReference(declared.items, spec) : undefined;
        if (isOpaqueJson(declared) || (items && isOpaqueJson(items)))
          throw unsupported(method, `an opaque JSON value as parameter ${JSON.stringify(parameter.name)}`);
        schemaType(parameter.schema, spec, `${method.operationId}.${parameter.name}`);
      }
      const request = requestContent(method, spec);
      if (stream === 'server' && request) {
        throw unsupported(method, 'SSE request body is not supported');
      }
      if (stream === 'unary') {
        for (const success of method.successes ?? []) responseContent(method, success, spec);
      } else if (operation?.messages?.output) {
        schemaType(operation.messages.output, spec, `${method.operationId}.messages.output`);
        if (duplex && operation.messages.input)
          schemaType(operation.messages.input, spec, `${method.operationId}.messages.input`);
      }
      if (!operation) throw unsupported(method, 'missing operation contract');
      const base = operationTypeBase(method.operationId);
      const groups = parameterGroups(method.parameters ?? []);
      if (groups.path.length)
        claimGeneratedSymbol(generatedSymbols, `${base}Path`, `${method.operationId} path parameters`);
      if (groups.query.length)
        claimGeneratedSymbol(generatedSymbols, `${base}Query`, `${method.operationId} query parameters`);
      if (groups.header.length)
        claimGeneratedSymbol(generatedSymbols, `${base}Headers`, `${method.operationId} header parameters`);
      if (stream === 'server' || duplex)
        claimGeneratedSymbol(generatedSymbols, `${base}Message`, `${method.operationId} stream message`);
      if (duplex) claimGeneratedSymbol(generatedSymbols, `${base}Send`, `${method.operationId} stream request message`);
      if (request?.schema) claimGeneratedSymbol(generatedSymbols, `${base}Body`, `${method.operationId} request body`);
      if (groups.path.length || groups.query.length || groups.header.length || request)
        claimGeneratedSymbol(generatedSymbols, `${base}Input`, `${method.operationId} request input`);
      if (stream === 'unary' && requiresResultEnvelope(method.successes ?? []))
        claimGeneratedSymbol(generatedSymbols, `${base}Result`, `${method.operationId} response result`);
      for (const error of operation.errors) {
        if (error.schema) schemaType(error.schema, spec, `${method.operationId}.errors.${error.code}`);
        const symbol = errorTypeName(method.operationId, error.code);
        const owner = `${service.className}.${method.operationId}.${error.code}@${error.status}`;
        claimGeneratedSymbol(generatedSymbols, symbol, `declared error ${owner}`);
        claimGeneratedSymbol(generatedSymbols, `is${symbol}`, `declared error guard ${owner}`);
      }
    }
  }
}

/**
 * Check one declared Connect transport.
 *
 * `protobufMethod` is the RPC path the provider published and the only path the
 * emitted client will use; a Connect entry without one, or with a codec no
 * first-party runtime implements, would emit a method that cannot address the
 * provider. A contract that declares Connect must also publish the descriptor
 * the binary codec reads.
 */
function validateConnectTransport(method: MethodIR, transport: ClientTransportContract, spec: SpecIR): void {
  if (transport.encoding !== 'json' && transport.encoding !== 'proto') {
    throw unsupported(method, `connect encoding ${JSON.stringify(transport.encoding)}`);
  }
  if (!transport.protobufMethod || !transport.path.startsWith('/')) {
    throw unsupported(method, 'a connect transport without a fully-qualified protobuf method path');
  }
  if (transport.encoding === 'proto' && !spec.contract?.protobuf) {
    throw unsupported(method, 'connect encoding "proto" without a published protobuf descriptor');
  }
}

/**
 * Check the semantics Connect narrows relative to REST.
 *
 * Two of them are real narrowings, and both fail generation rather than
 * emitting a client that loses information on its first call:
 *
 * 1. **One success.** Connect answers every successful RPC with HTTP 200, so a
 *    contract declaring several success statuses has no Connect representation.
 * 2. **Round-tripping field names.** A message field's JSON key is protobuf's
 *    `json_name`, derived from the snake-case wire name. A declared property
 *    whose name does not survive that derivation would arrive under a different
 *    key than it left under.
 */
function validateConnectSemantics(method: MethodIR, spec: SpecIR): void {
  if (method.client?.stream === 'unary' && (method.successes?.length ?? 0) > 1) {
    throw unsupported(method, 'a connect transport with more than one declared success status');
  }
  const seen = new Set<ClientSchema>();
  const check = (schema: ClientSchema | undefined, field: string): void => {
    if (!schema || seen.has(schema)) return;
    seen.add(schema);
    if (schema.$ref) {
      const name = schema.$ref.slice('#/components/schemas/'.length);
      check(spec.schemas?.[name], `components.schemas.${name}`);
      return;
    }
    // Protobuf has no null. A declared `null` would be written as nothing and
    // read back as the field's zero, so the caller could not tell "explicitly
    // null" from "0" or "". Refuse rather than emit that.
    if (schema.nullable) throw unsupported(method, `a nullable field (${field}), which protobuf cannot represent`);
    // Protobuf has no lossless form for a value the provider does not
    // interpret: google.protobuf.Value holds every number as a double.
    if (isOpaqueJson(schema) || schema.additionalProperties === true)
      throw unsupported(method, `an opaque JSON value (${field}), which protobuf cannot carry without loss`);
    for (const [name, child] of Object.entries(schema.properties ?? {})) {
      if (protoJsonName(toProtoSnakeCase(name)) !== name) {
        throw unsupported(
          method,
          `property ${JSON.stringify(name)} in ${field}, whose protobuf json_name would rename it`,
        );
      }
      check(child, `${field}.${name}`);
    }
    check(schema.items, `${field}.items`);
    if (typeof schema.additionalProperties === 'object') {
      check(schema.additionalProperties, `${field}.additionalProperties`);
    }
    for (const branch of schema.oneOf ?? []) check(branch, field);
  };
  for (const parameter of method.parameters ?? []) {
    if (protoJsonName(toProtoSnakeCase(parameter.name)) !== parameter.name) {
      throw unsupported(
        method,
        `parameter ${JSON.stringify(parameter.name)}, whose protobuf json_name would rename it`,
      );
    }
    check(parameter.schema, `${method.operationId}.${parameter.name}`);
  }
  for (const content of method.request?.content ?? []) check(content.schema, `${method.operationId}.request`);
  for (const success of method.successes ?? []) {
    for (const content of success.content) check(content.schema, `${method.operationId}.${success.status}`);
  }
  check(method.client?.messages?.output, `${method.operationId}.messages.output`);
}

function claimGeneratedSymbol(symbols: Map<string, string>, symbol: string, owner: string): void {
  const previous = symbols.get(symbol);
  if (previous) {
    throw new Error(
      `clientgen_unsupported_semantic: generated symbol ${symbol} collision between ${previous} and ${owner}`,
    );
  }
  symbols.set(symbol, owner);
}

function generateStrictTypes(spec: SpecIR): string {
  const hasErrors = spec.services.some((service) => service.methods.some((method) => method.client?.errors.length));
  const hasErrorSchemas = spec.services.some((service) =>
    service.methods.some((method) => method.client?.errors.some((error) => error.schema !== undefined)),
  );
  const lines = [
    '// Auto-generated by @putnami/client — do not edit',
    '',
    ...(hasErrors ? ["import { isClientFrameworkError } from '@putnami/client';", ''] : []),
    ...(hasErrorSchemas ? [`const CLIENT_ERROR_SCHEMAS = ${JSON.stringify(spec.schemas ?? {})} as const;`, ''] : []),
  ];
  for (const [name, schema] of Object.entries(spec.schemas ?? {})) {
    lines.push(`export type ${name} = ${schemaType(schema, spec, `components.schemas.${name}`)};`, '');
  }
  // Only a contract that declares a response cache has one to bypass, so every
  // other generated client keeps its bytes.
  const cached = spec.services.some(requiresResponseCache);
  lines.push(
    'export interface ClientCallOptions {',
    '  signal?: AbortSignal;',
    '  /** Trusted deployment endpoint; validated with the registered binding policy. */',
    '  endpoint?: string;',
    '  /** Also receives the declared JSON success body byte for byte, once the call accepted it. */',
    "  successBody?: import('@putnami/client').SuccessBody;",
    ...(cached
      ? [
          "  /** Ask for the provider's current answer: the declared response cache neither reads, stores nor joins a call in flight. */",
          '  withoutResponseCache?: boolean;',
        ]
      : []),
    '}',
    '',
  );
  for (const service of spec.services) {
    for (const method of service.methods) lines.push(...methodTypes(method, spec));
  }
  return `${lines.join('\n').trimEnd()}\n`;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one generated input/result surface must preserve every request and response variant
function methodTypes(method: MethodIR, spec: SpecIR): string[] {
  const base = operationTypeBase(method.operationId);
  const lines: string[] = [];
  const groups = parameterGroups(method.parameters ?? []);
  if (groups.path.length) lines.push(...objectAlias(`${base}Path`, groups.path, spec), '');
  if (groups.query.length) lines.push(...objectAlias(`${base}Query`, groups.query, spec), '');
  if (groups.header.length) lines.push(...objectAlias(`${base}Headers`, groups.header, spec), '');
  if (isStreamMode(method.client?.stream) && method.client?.messages?.output) {
    lines.push(
      `export type ${base}Message = ${schemaType(method.client.messages.output, spec, `${method.operationId}.messages.output`)};`,
      '',
    );
  }
  if (isDuplexMode(method.client?.stream) && method.client?.messages?.input) {
    lines.push(
      `export type ${base}Send = ${schemaType(method.client.messages.input, spec, `${method.operationId}.messages.input`)};`,
      '',
    );
  }
  const request = requestContent(method, spec);
  if (request?.binary) lines.push(`export type ${base}Body = ${binaryBodyType()};`, '');
  else if (request?.schema)
    lines.push(`export type ${base}Body = ${schemaType(request.schema, spec, `${method.operationId}.body`)};`, '');
  if (groups.path.length || groups.query.length || groups.header.length || request) {
    lines.push(`export interface ${base}Input {`);
    if (groups.path.length) lines.push(`  path: ${base}Path;`);
    if (groups.query.length) lines.push(`  query${allOptional(groups.query) ? '?' : ''}: ${base}Query;`);
    if (groups.header.length) lines.push(`  headers${allOptional(groups.header) ? '?' : ''}: ${base}Headers;`);
    if (request?.binary?.streamed) lines.push('  contentType: string;');
    if (request)
      lines.push(
        `  body${request.required ? '' : '?'}: ${request.schema || request.binary ? `${base}Body` : 'never'};`,
      );
    lines.push('}', '');
  }
  const octetResponse = binaryResponse(method);
  if (octetResponse) lines.push(...binaryPayloadTypeLines(base, method, octetResponse));
  const successes = method.successes ?? [];
  if (method.client?.stream === 'unary' && requiresResultEnvelope(successes)) {
    lines.push(`export type ${base}Result =`);
    for (const [index, success] of successes.entries()) {
      const content = responseContent(method, success, spec);
      const separator = index === successes.length - 1 ? ';' : '';
      lines.push(
        `  | { status: ${success.status}; data: ${content.schema ? schemaType(content.schema, spec, `${method.operationId}.${success.status}`) : 'void'}; headers: Headers }${separator}`,
      );
    }
    lines.push('');
  }
  for (const error of method.client?.errors ?? []) {
    const detail = error.schema
      ? schemaType(error.schema, spec, `${method.operationId}.errors.${error.code}`)
      : 'undefined';
    const errorType = errorTypeName(method.operationId, error.code);
    lines.push(
      `export type ${errorType} = import('@putnami/client').ClientFrameworkError<${JSON.stringify(error.code)}, ${detail}, ${JSON.stringify(spec.contract?.service.id)}, ${JSON.stringify(method.operationId)}, ${error.status}>;`,
      '',
      `export function is${errorType}(error: unknown): error is ${errorType} {`,
      `  return isClientFrameworkError(error, { service: ${JSON.stringify(spec.contract?.service.id)}, method: ${JSON.stringify(method.operationId)}, status: ${error.status}, code: ${JSON.stringify(error.code)}${error.schema ? `, detailsSchema: ${JSON.stringify(error.schema)}, schemas: CLIENT_ERROR_SCHEMAS` : ''} });`,
      '}',
    );
  }
  if (method.client?.errors.length) lines.push('');
  return lines;
}

function errorTypeName(operationId: string, code: string): string {
  return `${operationTypeBase(operationId)}${pascalCase(code.replace(/[^A-Za-z0-9]+/g, '_'))}Error`;
}

/** Whether one of the service's operations declares a response cache. */
function requiresResponseCache(service: ServiceIR): boolean {
  return service.methods.some((method) => method.client?.resilience?.cache !== undefined);
}

/**
 * The client runtime capabilities the service's operations need, in ascending
 * order, derived exactly as the Go emitter and the manifest derive them: the
 * response cache when an operation declares a cache policy, and SSE
 * continuation when one of its transports declares a continuation (ADR 0013).
 */
function serviceRuntimeCapabilities(service: ServiceIR): string[] {
  const cache = requiresResponseCache(service);
  const continuation = service.methods.some((method) =>
    (method.client?.transports ?? []).some((transport) => transport.sse?.continuation !== undefined),
  );
  return [...(cache ? ['response-cache'] : []), ...(continuation ? ['sse-continuation'] : [])];
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: service emission keeps generated imports, descriptor metadata, and methods synchronized
function generateStrictService(service: ServiceIR, spec: SpecIR): string {
  const contract = spec.contract;
  if (!contract) throw new Error('clientgen_first_party_required: strict emitter requires a contract');
  const operations = Object.fromEntries(service.methods.map((method) => [method.operationId, method.client]));
  const descriptor = {
    contract,
    service: service.name,
    operations,
    ...(spec.schemas ? { schemas: spec.schemas } : {}),
    transport: 'http',
  };
  const capabilities = serviceRuntimeCapabilities(service);
  const typeImports = new Set<string>(['ClientCallOptions']);
  for (const method of service.methods) {
    const base = operationTypeBase(method.operationId);
    const groups = parameterGroups(method.parameters ?? []);
    if (groups.path.length || groups.query.length || groups.header.length || method.request)
      typeImports.add(`${base}Input`);
    if (isStreamMode(method.client?.stream) && !isByteStream(method)) typeImports.add(`${base}Message`);
    if (isDuplexMode(method.client?.stream) && !isByteStream(method)) typeImports.add(`${base}Send`);
    const octetResponse = binaryResponse(method);
    if (octetResponse) typeImports.add(binaryPayloadTypeName(base));
    if (method.client?.stream === 'unary' && !octetResponse && requiresResultEnvelope(method.successes ?? []))
      typeImports.add(`${base}Result`);
    const success = method.successes?.[0];
    if (
      method.client?.stream === 'unary' &&
      !octetResponse &&
      success &&
      !requiresResultEnvelope(method.successes ?? [])
    ) {
      collectReferencedType(responseContent(method, success, spec).schema, typeImports);
    }
  }
  const lines = [
    '// Auto-generated by @putnami/client — do not edit',
    '',
    // The canonicalizer prunes what a service does not use, so this line names
    // every symbol the emitter can reach rather than tracking which it did.
    `import { BaseClient, bindServiceClient, encodeHttpParameter, readBoundedBody, registerServiceClient, type ByteStream, type ClientConfig, type DuplexStream, type FrameStream, type GeneratedServiceDescriptor, type ServiceBinding, type ServiceClientBindingTarget, type StreamObserver } from '@putnami/client';`,
    ...(capabilities.length ? ["import { requireClientRuntimeCapabilities } from '@putnami/client';"] : []),
    `import type { ${[...typeImports].sort().join(', ')} } from './types';`,
    '',
    `const SERVICE_DESCRIPTOR = ${JSON.stringify(descriptor)} as const satisfies GeneratedServiceDescriptor;`,
    '',
    ...(capabilities.length
      ? [
          '// The operations below need these client runtime capabilities. A runtime that',
          '// predates one fails to load this module instead of running without it.',
          `requireClientRuntimeCapabilities([${capabilities.map((capability) => `'${capability}'`).join(', ')}]);`,
          '',
        ]
      : []),
    `export class ${service.className} extends BaseClient {`,
    `  readonly serviceName = ${JSON.stringify(contract.service.id)};`,
    '',
    '  constructor(config: ClientConfig) {',
    '    super({',
    '      ...config,',
    `      serviceId: ${JSON.stringify(contract.service.id)},`,
    '      operationContracts: SERVICE_DESCRIPTOR.operations,',
    // A contract that declares Connect embeds the provider's own descriptor, so
    // the binary codec reads field numbers and presence from the provider
    // rather than re-deriving them.
    ...(contract.protobuf ? ['      clientProtobuf: SERVICE_DESCRIPTOR.contract.protobuf,'] : []),
    ...(contract.defaults?.resilience
      ? ['      clientDefaults: SERVICE_DESCRIPTOR.contract.defaults?.resilience,']
      : []),
    ...(spec.schemas ? ['      clientSchemas: SERVICE_DESCRIPTOR.schemas,'] : []),
    '    });',
    '  }',
  ];
  for (const method of service.methods) {
    const rendered = renderMethod(method, spec);
    const call = `this.forEndpoint(options.endpoint).${method.name}(${methodTakesInput(method) ? 'input, ' : ''}{ ...options, endpoint: undefined })`;
    const endpointSelection =
      isStreamMode(method.client?.stream) && !isByteStream(method)
        ? [
            '      try {',
            `        return ${call};`,
            '      } catch (error) {',
            `        return ${renderFailedEndpointStream(method)};`,
            '      }',
          ]
        : [`      return ${call};`];
    rendered.splice(1, 0, '    if (options?.endpoint !== undefined) {', ...endpointSelection, '    }');
    lines.push('', ...rendered);
  }
  lines.push(
    '}',
    '',
    `/** Bind caller-resolved endpoints; reuse for this owner, then dispose. */`,
    `export function bind${service.className}(binding: ServiceBinding): ${service.className} {`,
    `  return bindServiceClient(${service.className}, SERVICE_DESCRIPTOR, binding);`,
    '}',
    '',
    `export function register${service.className}(target: ServiceClientBindingTarget, binding?: ServiceBinding): ServiceClientBindingTarget {`,
    `  return registerServiceClient(target, ${service.className}, SERVICE_DESCRIPTOR, binding);`,
    '}',
    '',
  );
  return lines.join('\n');
}

function renderMethod(method: MethodIR, spec: SpecIR): string[] {
  const base = operationTypeBase(method.operationId);
  const args = `${methodTakesInput(method) ? `input: ${base}Input, ` : ''}options?: ClientCallOptions`;
  if (method.client?.stream === 'server') return renderServerStreamMethod(method, base, args);
  if (providerWireOf(method)) return renderProviderWireMethod(method, base, args);
  if (isDuplexMode(method.client?.stream)) return renderDuplexStreamMethod(method, base, args);
  const successes = method.successes ?? [];
  const octetResponse = binaryResponse(method);
  const envelope = !octetResponse && requiresResultEnvelope(successes);
  const returnType = octetResponse
    ? binaryPayloadTypeName(base)
    : envelope
      ? `${base}Result`
      : successes[0]
        ? responseType(method, successes[0], spec)
        : 'void';
  const lines = [`  async ${method.name}(${args}): Promise<${returnType}> {`];
  const groups = parameterGroups(method.parameters ?? []);
  const requestParts: string[] = [];
  if (groups.path.length) requestParts.push(`      params: ${renderParameterValues(groups.path, 'input.path')},`);
  if (groups.query.length) requestParts.push(`      query: ${renderParameterValues(groups.query, 'input.query')},`);
  if (groups.header.length)
    requestParts.push(`      headers: ${renderParameterValues(groups.header, 'input.headers')},`);
  const request = requestContent(method, spec);
  if (request?.binary) requestParts.push(...binaryRequestParts(request.binary));
  else if (request) {
    requestParts.push('      body: input.body,');
    if (request.schema) requestParts.push(`      requestSchema: ${JSON.stringify(request.schema)},`);
  }
  requestParts.push(
    `      operationId: ${JSON.stringify(method.operationId)},`,
    '      signal: options?.signal,',
    '      successBody: options?.successBody,',
  );
  if (method.client?.resilience?.cache) requestParts.push('      withoutResponseCache: options?.withoutResponseCache,');
  requestParts.push(`      successes: ${JSON.stringify(successes)},`);
  if (octetResponse) requestParts.push(...binaryResponseParts(octetResponse));
  // The result union is passed explicitly: every declared success variant stays
  // reachable and narrowable by status, instead of collapsing to the first 2xx.
  const call = octetResponse
    ? `${octetResponse.streamed ? 'requestBinaryStream' : 'requestBinary'}<${(successes[0]?.status ?? 200).toString()}>`
    : envelope
      ? `requestResult<${base}Result>`
      : 'request';
  lines.push(
    `    return this.${call}('${method.httpMethod}', ${JSON.stringify(method.path)}, {`,
    ...requestParts,
    '    });',
    '  }',
  );
  return lines;
}

/** The operation's provider-owned WebSocket wire (ADR 0010), if it declares one. */
function providerWireOf(method: MethodIR): ClientTransportContract | undefined {
  return method.client?.transports.find(
    (transport) => transport.protocol === 'websocket' && transport.websocket?.wire === 'provider',
  );
}

/** A provider-owned wire whose messages are raw octets. */
function isByteStream(method: MethodIR): boolean {
  return providerWireOf(method)?.encoding === 'binary';
}

// A provider-owned wire states its shape and nothing of its vocabulary: a byte
// stream resolves to a readable and writable pair once the provider accepted
// the upgrade, and a typed wire returns a frame handle of the declared message
// types. The runtime owns the socket, the credentials on the upgrade, the
// budgets, the heartbeat and the close codes.
function renderProviderWireMethod(method: MethodIR, base: string, args: string): string[] {
  const bytes = isByteStream(method);
  const returnType = bytes ? 'Promise<ByteStream>' : `FrameStream<${base}Send, ${base}Message>`;
  const entrypoint = bytes ? 'serviceByteStream' : `serviceFrameStream<${base}Send, ${base}Message>`;
  return [
    `  ${bytes ? 'async ' : ''}${method.name}(${args}): ${returnType} {`,
    `    return this.${entrypoint}('${method.httpMethod}', ${JSON.stringify(method.path)}, {`,
    ...streamRequestParts(method),
    '    });',
    '  }',
  ];
}

/** Whether the emitted method signature and recursive endpoint call carry input. */
function methodTakesInput(method: MethodIR): boolean {
  return Boolean(method.parameters?.length || (method.client?.stream === 'unary' && method.request));
}

/** A typed failed-stream expression for a synchronous endpoint-selection refusal. */
function renderFailedEndpointStream(method: MethodIR): string {
  const base = operationTypeBase(method.operationId);
  if (method.client?.stream === 'server') return `this.failedEndpointStream<never, ${base}Message>(error)`;
  return `this.failedEndpointStream<${base}Send, ${base}Message>(error)`;
}

/** A declared stream mode, of any direction. */
function isStreamMode(stream: string | undefined): boolean {
  return stream === 'server' || isDuplexMode(stream);
}

/** A stream the caller also writes to: client-streaming or bidirectional. */
function isDuplexMode(stream: string | undefined): boolean {
  return stream === 'client' || stream === 'bidirectional';
}

// A stream method carries the path, the query and the ordinary headers only.
// Identity, credentials and propagation context travel in the admission frame,
// which is why a browser opens the same socket with no header of its own.
function streamRequestParts(method: MethodIR): string[] {
  const groups = parameterGroups(method.parameters ?? []);
  const requestParts: string[] = [];
  if (groups.path.length) requestParts.push(`      params: ${renderParameterValues(groups.path, 'input.path')},`);
  if (groups.query.length) requestParts.push(`      query: ${renderParameterValues(groups.query, 'input.query')},`);
  if (groups.header.length)
    requestParts.push(`      headers: ${renderParameterValues(groups.header, 'input.headers')},`);
  // A stream never delivers a success body; the option travels so the runtime
  // refuses it before a socket opens instead of ignoring it.
  requestParts.push(
    `      operationId: ${JSON.stringify(method.operationId)},`,
    '      signal: options?.signal,',
    '      successBody: options?.successBody,',
  );
  return requestParts;
}

// `serviceStream` resolves SSE and WebSocket from the declared transport order
// at call time, so a server stream emits one entrypoint whichever transport the
// provider declares first. The emitter states the shape, not the carrier.
function renderServerStreamMethod(method: MethodIR, base: string, args: string): string[] {
  return [
    `  ${method.name}(${args}): StreamObserver<${base}Message> {`,
    `    return this.serviceStream('${method.httpMethod}', ${JSON.stringify(method.path)}, {`,
    ...streamRequestParts(method),
    '    });',
    '  }',
  ];
}

// A client stream and a bidirectional stream differ in what the provider is
// allowed to send back, not in the handle the caller holds: both return a
// `DuplexStream`, and the single declared result arrives as the last message
// before completion. Emitting two handle shapes would make an application that
// moves an operation from one mode to the other rewrite its call site.
function renderDuplexStreamMethod(method: MethodIR, base: string, args: string): string[] {
  const entrypoint = method.client?.stream === 'client' ? 'serviceClientStream' : 'serviceBidiStream';
  return [
    `  ${method.name}(${args}): DuplexStream<${base}Send, ${base}Message> {`,
    `    return this.${entrypoint}('${method.httpMethod}', ${JSON.stringify(method.path)}, {`,
    ...streamRequestParts(method),
    '    });',
    '  }',
  ];
}

function objectAlias(name: string, parameters: ParameterIR[], spec: SpecIR): string[] {
  const lines = [`export interface ${name} {`];
  for (const parameter of parameters) {
    lines.push(
      `  ${propertyKey(parameter.name)}${parameter.required ? '' : '?'}: ${schemaType(parameter.schema, spec, `${name}.${parameter.name}`)};`,
    );
  }
  lines.push('}');
  return lines;
}

function renderParameterValues(parameters: ParameterIR[], root: string): string {
  return `{ ${parameters
    .map((parameter) => {
      // An optional group is `input.query?.["x"]`: the optional-chaining form of
      // a computed member access carries the dot. `input.query?["x"]` is not
      // TypeScript, and Biome rejects the emitted file rather than the contract.
      const access = `${root}${parameter.required ? '' : '?.'}[${JSON.stringify(parameter.name)}]`;
      const requiredPath = parameter.location === 'path' && parameter.required ? ' as string' : '';
      return `${JSON.stringify(parameter.name)}: encodeHttpParameter(${access}, ${JSON.stringify(parameter.schema)}, { location: ${JSON.stringify(parameter.location)} })${requiredPath}`;
    })
    .join(', ')} }`;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: fail-closed schema rendering covers the complete supported neutral schema union
function schemaType(schema: ClientSchema, spec: SpecIR, field: string): string {
  const nullable = schema.nullable ? ' | null' : '';
  if (schema.$ref) {
    const prefix = '#/components/schemas/';
    if (!schema.$ref.startsWith(prefix)) throw new Error(`clientgen_unsupported_semantic: ${field} has external $ref`);
    const name = schema.$ref.slice(prefix.length);
    if (!spec.schemas?.[name]) throw new Error(`clientgen_unsupported_semantic: ${field} references missing ${name}`);
    assertSafeIdentifier(name, field);
    return `${name}${nullable}`;
  }
  // An opaque value is carried without interpretation (clientcontract ADR 0008).
  if (isOpaqueJson(schema)) return 'unknown';
  if (schema.oneOf) return `${schema.oneOf.map((branch) => schemaType(branch, spec, field)).join(' | ')}${nullable}`;
  if (schema.enum) {
    return `${schema.enum.map((value) => enumLiteral(value, schema)).join(' | ')}${nullable}`;
  }
  let type: string;
  switch (schema.type) {
    case 'string':
      type = schema.format === 'byte' || schema.format === 'binary' ? 'Uint8Array' : 'string';
      break;
    case 'integer':
      // D0.2: an integer's width is declared, never inferred. Choosing `number`
      // for an unformatted integer silently narrows a 64-bit provider field to
      // 53 bits, so the generator refuses instead of choosing.
      if (!INTEGER_FORMATS.has(schema.format ?? ''))
        throw new Error(
          `clientgen_unsupported_semantic: ${field} declares type "integer" without an int32|int64|uint32|uint64 format`,
        );
      type = schema.format === 'int64' || schema.format === 'uint64' ? 'bigint' : 'number';
      break;
    case 'number':
      type = 'number';
      break;
    case 'boolean':
      type = 'boolean';
      break;
    case 'array':
      if (!schema.items) throw new Error(`clientgen_unsupported_semantic: ${field} array has no items`);
      type = `Array<${schemaType(schema.items, spec, `${field}.items`)}>`;
      break;
    case 'object': {
      const properties = Object.entries(schema.properties ?? {});
      const required = new Set(schema.required ?? []);
      const members = properties.map(
        ([name, child]) =>
          `${propertyKey(name)}${required.has(name) ? '' : '?'}: ${schemaType(child, spec, `${field}.${name}`)}`,
      );
      if (typeof schema.additionalProperties === 'object') {
        const values = schemaType(schema.additionalProperties, spec, `${field}.additionalProperties`);
        type = members.length ? `({ ${members.join('; ')} } & Record<string, ${values}>)` : `Record<string, ${values}>`;
      } else if (schema.additionalProperties === true) {
        // A free-form object: every member is an opaque JSON value. Named
        // properties beside open members stay unemittable, as in Go.
        if (members.length)
          throw new Error(
            `clientgen_unsupported_semantic: ${field} mixes named properties and untyped additionalProperties`,
          );
        type = 'Record<string, unknown>';
      } else {
        type = members.length ? `{ ${members.join('; ')} }` : 'Record<string, never>';
      }
      break;
    }
    default:
      throw new Error(`clientgen_unsupported_semantic: ${field} has no representable schema type`);
  }
  return `${type}${nullable}`;
}

/** Whether a schema declares an opaque JSON value (`x-putnami-json: any`). */
function isOpaqueJson(schema: ClientSchema): boolean {
  return schema['x-putnami-json'] !== undefined;
}

/**
 * Follow local component references to the schema they name. A missing or
 * cyclic reference resolves to the last schema reached; the reader and
 * schemaType already refuse both.
 */
function resolveReference(schema: ClientSchema, spec: SpecIR): ClientSchema {
  const prefix = '#/components/schemas/';
  const seen = new Set<string>();
  let current = schema;
  for (let ref = current.$ref; ref?.startsWith(prefix); ref = current.$ref) {
    const name = ref.slice(prefix.length);
    const target = spec.schemas?.[name];
    if (!target || seen.has(name)) return current;
    seen.add(name);
    current = target;
  }
  return current;
}

function enumLiteral(value: string | number | boolean | { $number: string }, schema: ClientSchema): string {
  if (typeof value === 'object') {
    return schema.format === 'int64' || schema.format === 'uint64' ? `${value.$number}n` : value.$number;
  }
  return JSON.stringify(value);
}

function requestContent(
  method: MethodIR,
  spec: SpecIR,
): { required: boolean; schema?: ClientSchema; binary?: ContentIR } | undefined {
  if (!method.request) return undefined;
  const octets = binaryRequest(method);
  if (octets) return { required: method.request.required, binary: octets };
  if (method.request.content.length !== 1 || method.request.content[0].mediaType !== 'application/json') {
    throw unsupported(method, 'request content types other than one application/json representation');
  }
  const content = method.request.content[0];
  if (content.schema) schemaType(content.schema, spec, `${method.operationId}.request`);
  return { required: method.request.required, schema: content.schema };
}

function responseContent(
  method: MethodIR,
  success: SuccessIR,
  spec: SpecIR,
): { schema?: ClientSchema; binary?: ContentIR } {
  if (success.content.length === 0) return {};
  const octets = success.content.find(isBinaryContent);
  if (octets) return { binary: octets };
  if (success.content.length !== 1 || success.content[0].mediaType !== 'application/json') {
    throw unsupported(
      method,
      `response ${success.status} content types other than one application/json representation`,
    );
  }
  const content = success.content[0];
  if (content.schema) schemaType(content.schema, spec, `${method.operationId}.responses.${success.status}`);
  return { schema: content.schema };
}

// ---------------------------------------------------------------------------
// Raw octet emission
//
// A binary operation shares nothing with a JSON one past the request line: no
// request schema to project, no response schema to decode, no encoding step.
// Everything the emitter does differently for octets lives in this section, so
// the JSON branches above keep exactly one meaning.
//
// The emitted shape, and why:
//   - the request payload is `Uint8Array | ArrayBuffer | ReadableStream`, read
//     under the declared bound before a socket is opened. A caller streams from
//     a file or a buffer, and an oversized source is refused without being read
//     to the end.
//   - the success payload is a named type carrying the octets, the status the
//     provider answered and the content type it labeled them with. A caller
//     that had to re-read the media type from a header would be writing
//     transport code the contract already declares.
//   - nothing is base64-encoded and nothing is wrapped in JSON.
// ---------------------------------------------------------------------------

/** A raw octet representation is exactly `{ type: "string", format: "binary" }`. */
function isBinaryContent(content: ContentIR): boolean {
  return content.schema?.type === 'string' && content.schema.format === 'binary';
}

/** The declared raw octet request representation of an operation, or undefined. */
function binaryRequest(method: MethodIR): ContentIR | undefined {
  return method.request?.content.find(isBinaryContent);
}

/** The declared raw octet success representation of an operation, or undefined. */
function binaryResponse(method: MethodIR): ContentIR | undefined {
  for (const success of method.successes ?? []) {
    const content = success.content.find(isBinaryContent);
    if (content) return content;
  }
  return undefined;
}

/** True when either side of an operation carries raw octets. */
function carriesBinary(method: MethodIR): boolean {
  return binaryRequest(method) !== undefined || binaryResponse(method) !== undefined;
}

/**
 * Refuse raw octets everywhere they cannot travel unchanged, at generation
 * time, naming the operation.
 */
function validateBinaryOperation(method: MethodIR): void {
  if (!carriesBinary(method)) return;
  const streamed = binaryRequest(method)?.streamed || binaryResponse(method)?.streamed;
  if (streamed && method.client?.resilience?.cache) {
    throw unsupported(method, 'a response cache on a raw HTTP stream');
  }
  for (const content of [binaryRequest(method), binaryResponse(method)]) {
    if (
      content?.streamed !== undefined &&
      (content.streamed !== true ||
        content.mediaType !== '*/*' ||
        !Number.isSafeInteger(content.maxBytes) ||
        (content.maxBytes ?? 0) <= 0)
    ) {
      throw unsupported(method, 'streamed octets require wildcard media type and a positive byte bound');
    }
  }
  if (method.client && method.client.stream !== 'unary') {
    throw unsupported(method, 'a raw octet payload on a stream; streams carry declared messages, not bodies');
  }
  const transport = method.client?.transports[0];
  if (transport && transport.protocol !== 'rest-json') {
    // A Connect envelope carries one encoded message, so octets would have to
    // travel base64 inside it — the silent re-wrapping this declaration refuses.
    throw unsupported(
      method,
      `a raw octet payload dispatched on ${transport.protocol}; only rest-json carries octets unchanged`,
    );
  }
  const successes = method.successes ?? [];
  if (binaryResponse(method) && successes.length !== 1) {
    throw unsupported(method, 'a raw octet payload beside multiple declared success variants');
  }
  for (const success of successes) {
    if (success.content.some(isBinaryContent) && success.content.length !== 1) {
      throw unsupported(method, `response ${success.status} mixing raw octets with another representation`);
    }
    if (success.headers?.length && success.content.some(isBinaryContent)) {
      throw unsupported(method, `response ${success.status} declaring typed headers beside raw octets`);
    }
  }
  const request = binaryRequest(method);
  if (request && method.request && method.request.content.length !== 1) {
    throw unsupported(method, 'a raw octet request mixed with another representation');
  }
}

/** The emitted TypeScript type of a raw octet request payload. */
function binaryBodyType(): string {
  return "import('@putnami/client').BinarySource";
}

/** The emitted payload type name of a raw octet success. */
function binaryPayloadTypeName(base: string): string {
  return `${base}Payload`;
}

/** Declare the named payload type a binary success hands back. */
function binaryPayloadTypeLines(base: string, method: MethodIR, content: ContentIR): string[] {
  const success = (method.successes ?? [])[0];
  return [
    `export interface ${binaryPayloadTypeName(base)} {`,
    `  /** The declared success status, ${success?.status ?? 200}. */`,
    `  readonly status: ${success?.status ?? 200};`,
    content.streamed
      ? '  /** The concrete media type the provider labeled the payload with. */'
      : `  /** The media type the provider labeled the payload with, checked against the declared ${content.mediaType}. */`,
    '  readonly contentType: string;',
    content.streamed
      ? '  /** Consume or cancel the raw body to release the connection. */'
      : `  /** The payload, verbatim. It is read under the declared ${content.maxBytes ?? 0}-octet bound. */`,
    content.streamed ? '  readonly body: ReadableStream<Uint8Array>;' : '  readonly body: Uint8Array;',
    '}',
    '',
  ];
}

/** The request parts a binary operation adds to the emitted call. */
function binaryRequestParts(request: ContentIR): string[] {
  if (request.streamed)
    return [
      '      body: input.body,',
      '      requestMediaType: input.contentType,',
      '      streamedRequest: true,',
      `      maxRequestBytes: ${request.maxBytes ?? 0},`,
    ];
  return [
    `      body: await readBoundedBody(input.body, ${request.maxBytes ?? 0}),`,
    `      requestMediaType: ${JSON.stringify(request.mediaType)},`,
  ];
}

/** The response parts a binary operation adds to the emitted call. */
function binaryResponseParts(response: ContentIR): string[] {
  if (response.streamed) return ['      streamedResponse: true,', `      maxPayloadBytes: ${response.maxBytes ?? 0},`];
  return [
    `      responseMediaType: ${JSON.stringify(response.mediaType)},`,
    `      maxPayloadBytes: ${response.maxBytes ?? 0},`,
  ];
}

function responseType(method: MethodIR, success: SuccessIR, spec: SpecIR): string {
  const schema = responseContent(method, success, spec).schema;
  return schema ? schemaType(schema, spec, `${method.operationId}.responses.${success.status}`) : 'void';
}

function parameterGroups(parameters: ParameterIR[]): Record<ParameterIR['location'], ParameterIR[]> {
  return {
    path: parameters.filter((parameter) => parameter.location === 'path'),
    query: parameters.filter((parameter) => parameter.location === 'query'),
    header: parameters.filter((parameter) => parameter.location === 'header'),
  };
}

function allOptional(parameters: ParameterIR[]): boolean {
  return parameters.every((parameter) => !parameter.required);
}

function requiresResultEnvelope(successes: SuccessIR[]): boolean {
  return successes.length !== 1 || Boolean(successes[0]?.headers?.length);
}

function collectReferencedType(schema: ClientSchema | undefined, target: Set<string>): void {
  if (!schema) return;
  if (schema.$ref) {
    target.add(schema.$ref.slice('#/components/schemas/'.length));
    return;
  }
  for (const branch of schema.oneOf ?? []) collectReferencedType(branch, target);
  if (schema.items) collectReferencedType(schema.items, target);
  for (const property of Object.values(schema.properties ?? {})) collectReferencedType(property, target);
  if (typeof schema.additionalProperties === 'object') collectReferencedType(schema.additionalProperties, target);
}

function unsupported(method: MethodIR, detail: string): Error {
  return new Error(`clientgen_unsupported_semantic: operation ${method.operationId} uses ${detail}`);
}

function propertyKey(name: string): string {
  return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name) ? name : JSON.stringify(name);
}

function generateStrictIndex(spec: SpecIR): string {
  const lines = ['// Auto-generated by @putnami/client — do not edit', '', `export * from './types';`];
  for (const service of spec.services) {
    lines.push(
      `export { ${service.className}, register${service.className}, bind${service.className} } from './${toKebabCase(service.className)}';`,
    );
  }
  return `${lines.join('\n')}\n`;
}

function generateStrictPackage(options: TsGeneratorOptions): string {
  return `${JSON.stringify(
    {
      name: options.packageName,
      version: options.version ?? '0.0.1',
      description: options.description ?? `Generated API client for ${options.packageName}`,
      main: 'src/index.ts',
      exports: { '.': './src/index.ts' },
      dependencies: { '@putnami/client': options.clientDependency ?? 'workspace:*' },
      private: false,
    },
    null,
    2,
  )}\n`;
}
