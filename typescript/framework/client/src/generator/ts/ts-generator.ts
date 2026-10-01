import type { FieldIR, MethodIR, ServiceIR, SpecIR } from '../ir.type';
import { assertSafeIdentifier, operationTypeBase, toKebabCase } from '../string-utils';
import { generateStrictTypeScriptClient } from './strict-ts-generator';

/**
 * Options for the TypeScript client generator.
 */
export interface TsGeneratorOptions {
  /** npm package name for the generated client (e.g. "@myorg/users-api-client") */
  packageName: string;
  /** Version for the generated package.json */
  version?: string;
  /** Description for the generated package.json */
  description?: string;
  /**
   * Relative path the generated `tsconfig.json` will `extends:`. The caller
   * computes this against the actual output directory so the path is
   * correct no matter how deeply the output dir sits under the workspace
   * root. Standalone callers default to
   * `../../../../tsconfig.base.json`; workspace generation passes a computed
   * value.
   */
  tsconfigExtends?: string;
  /**
   * Dependency specifier the generated `package.json` pins `@putnami/client`
   * with. The framework workspace holds the package as a member
   * (`workspace:*`, the default); a consumer workspace that takes it from a
   * catalog passes `catalog:`. See `resolveClientDependencySpec`.
   */
  clientDependency?: string;
  /**
   * Per-operation producer attribution embedded into the generated descriptor.
   * Entries are joined onto the spec by HTTP method + path; an operation with no
   * entry is emitted unattributed rather than inheriting a neighbour's feature.
   */
  design?: {
    operations: Array<{
      method: string;
      path: string;
      producerProject: string;
      producerFeature: string;
    }>;
  };
}

/**
 * Represents a generated file.
 */
export interface GeneratedFile {
  path: string;
  content: string;
}

/**
 * Generate TypeScript client code from a SpecIR.
 *
 * Produces:
 * - One client class file per service
 * - A types.ts file with all request/response interfaces
 * - An index.ts that re-exports everything
 * - A package.json for the generated client
 */
export function generateTypeScriptClient(spec: SpecIR, options: TsGeneratorOptions): GeneratedFile[] {
  if (spec.contract) {
    return generateStrictTypeScriptClient(spec, options);
  }
  // The spec is untrusted input. Every spec-derived name that will be emitted
  // in an identifier position is validated up front, before any code is
  // rendered — a hostile name aborts generation instead of injecting code.
  assertSpecIdentifiers(spec);

  const files: GeneratedFile[] = [];

  // Generate types.ts with all interfaces
  files.push({
    path: 'src/types.ts',
    content: generateTypesFile(spec),
  });

  // Generate one client file per service
  for (const service of spec.services) {
    files.push({
      path: `src/${toKebabCase(service.className)}.ts`,
      content: generateClientFile(service, spec, options),
    });
  }

  // Generate index.ts
  files.push({
    path: 'src/index.ts',
    content: generateIndexFile(spec),
  });

  // Generate package.json
  files.push({
    path: 'package.json',
    content: generatePackageJson(options),
  });

  // Generate tsconfig.json
  const tsconfigExtends = options.tsconfigExtends ?? '../../../../tsconfig.base.json';
  files.push({
    path: 'tsconfig.json',
    content: `${JSON.stringify({ extends: tsconfigExtends, compilerOptions: {} }, null, 2)}\n`,
  });

  return files;
}

// ---------------------------------------------------------------------------
// Identifier safety
// ---------------------------------------------------------------------------

/**
 * Validate every spec-derived name that this generator interpolates in an
 * identifier position (never escapable, unlike string literals which go
 * through {@link sanitizeStringLiteral}):
 *
 * - service class names (class declaration, index re-export, kebab-cased file name)
 * - method names
 * - the `operationTypeBase(operationId)` base of the per-operation
 *   `…Params` / `…Query` / `…Body` / `…Response` interface names: it is legal
 *   by construction, so what is checked is that no two operations share it
 * - path/query/body/response field names (interface members, object keys, and
 *   `params.<name>` / `query.<name>` property access)
 * - named-type names and their fields
 * - type tokens (`tsType`, `bodyType`, `responseType`)
 *
 * Throws on the first unsafe name, so a hostile spec aborts generation instead
 * of injecting executable code into the consumer's app.
 */
function assertSpecIdentifiers(spec: SpecIR): void {
  assertReusableTypeIdentifiers(spec);
  for (const service of spec.services) assertServiceIdentifiers(service);
  assertDistinctOperationTypeBases(spec);
}

/**
 * The per-operation interfaces of every service land in one `types.ts`, and
 * `operationTypeBase` is not injective: ids that differ only in punctuation
 * (`get_Widgets_Id` and `get.widgets_Id`, `a$b` and `ab`) share a base. Emitting
 * both would declare the same interface twice, which surfaces later as a
 * duplicate-identifier error over the whole generated project, so fail here
 * with the pair instead.
 */
function assertDistinctOperationTypeBases(spec: SpecIR): void {
  const bases = new Map<string, string>();
  for (const service of spec.services) {
    for (const method of service.methods) {
      const base = operationTypeBase(method.operationId);
      const previous = bases.get(base);
      if (previous !== undefined && previous !== method.operationId) {
        throw new Error(
          `Operations ${JSON.stringify(previous)} and ${JSON.stringify(method.operationId)} both generate the type name ` +
            `${JSON.stringify(base)}; rename one operationId so the two produce distinct TypeScript symbols.`,
        );
      }
      bases.set(base, method.operationId);
    }
  }
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: every reusable identifier category needs its own context
function assertReusableTypeIdentifiers(spec: SpecIR): void {
  if (spec.namedTypes) {
    for (const [name, fields] of Object.entries(spec.namedTypes)) {
      assertSafeIdentifier(name, 'named type');
      assertSafeFields(fields, `named type ${JSON.stringify(name)}`);
    }
  }
  if (spec.enums) {
    for (const name of Object.keys(spec.enums)) assertSafeIdentifier(name, 'enum type');
  }
  if (spec.unions) {
    for (const [name, union] of Object.entries(spec.unions)) {
      assertSafeIdentifier(name, 'union type');
      assertSafeIdentifier(union.discriminator, `discriminator of union ${JSON.stringify(name)}`);
      for (const variant of union.variants) {
        if (variant.fields) assertSafeFields(variant.fields, `variant ${JSON.stringify(variant.tag)} of union ${name}`);
      }
    }
  }
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: every generated symbol position is validated locally
function assertServiceIdentifiers(service: ServiceIR): void {
  assertSafeIdentifier(service.className, `client class name of service ${JSON.stringify(service.name)}`);

  // Symbol normalization is not injective: two canonical operation ids that
  // differ only in punctuation collapse onto one method name. Emitting both
  // would produce a class with a duplicated member, so fail with the pair
  // rather than shipping source that does not compile.
  const symbols = new Map<string, string>();
  for (const method of service.methods) {
    const where = `operation ${JSON.stringify(method.operationId)}`;
    assertSafeIdentifier(method.name, `method name of ${where}`);
    const previous = symbols.get(method.name);
    if (previous !== undefined && previous !== method.operationId) {
      throw new Error(
        `Operations ${JSON.stringify(previous)} and ${JSON.stringify(method.operationId)} both generate the method ` +
          `${JSON.stringify(method.name)}; rename one operationId so the two produce distinct TypeScript symbols.`,
      );
    }
    symbols.set(method.name, method.operationId);
    if (method.params) assertSafeFields(method.params, `path parameter of ${where}`);
    if (method.query) assertSafeFields(method.query, `query parameter of ${where}`);
    if (method.body) assertSafeFields(method.body, `body field of ${where}`);
    if (method.response) assertSafeFields(method.response, `response field of ${where}`);
    if (method.bodyType) assertSafeTypeToken(method.bodyType, `body type of ${where}`);
    if (method.responseType) assertSafeTypeToken(method.responseType, `response type of ${where}`);
  }
}

function assertSafeFields(fields: FieldIR[], context: string): void {
  for (const field of fields) {
    assertSafeIdentifier(field.name, context);
    assertSafeTypeToken(field.tsType, `type of field ${JSON.stringify(field.name)} (${context})`);
  }
}

/**
 * A type token must be a plain identifier (a primitive or the name of a shared
 * named type) or the exact `Record<string, unknown>` produced by the readers'
 * inline-object degrade rule — anything else could splice code into the
 * generated source.
 */
function assertSafeTypeToken(tsType: string, context: string): void {
  if (tsType === 'Record<string, unknown>') return;
  assertSafeIdentifier(tsType, context);
}

// ---------------------------------------------------------------------------
// Types file
// ---------------------------------------------------------------------------

function generateTypesFile(spec: SpecIR): string {
  const lines: string[] = ['// Auto-generated by @putnami/client — do not edit', ''];

  lines.push(...renderEnumDeclarations(spec.enums));
  lines.push(...renderUnionDeclarations(spec.unions));

  // Shared named types first — reused models referenced (by name) from the
  // per-operation interfaces and method signatures below.
  if (spec.namedTypes) {
    for (const [name, fields] of Object.entries(spec.namedTypes)) {
      lines.push(...renderInterface(name, fields));
      lines.push('');
    }
  }

  for (const service of spec.services) {
    for (const method of service.methods) {
      lines.push(...renderMethodTypeDeclarations(method));
    }
  }

  return lines.join('\n');
}

function renderEnumDeclarations(enums: SpecIR['enums']): string[] {
  const lines: string[] = [];
  for (const [name, values] of Object.entries(enums ?? {})) {
    lines.push(`export type ${name} = ${values.map((value) => `'${sanitizeStringLiteral(value)}'`).join(' | ')};`);
    lines.push('');
  }
  return lines;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: compatibility unions retain all optional field variants
function renderUnionDeclarations(unions: SpecIR['unions']): string[] {
  const lines: string[] = [];
  for (const [name, union] of Object.entries(unions ?? {})) {
    lines.push(`export type ${name} =`);
    for (const [index, variant] of union.variants.entries()) {
      lines.push(`  | {`);
      lines.push(`      ${union.discriminator}: '${sanitizeStringLiteral(variant.tag)}';`);
      for (const field of variant.fields ?? []) {
        const opt = field.optional ? '?' : '';
        const type = field.array ? `${field.tsType}[]` : field.tsType;
        lines.push(`      ${field.name}${opt}: ${type};`);
      }
      lines.push(`    }${index === union.variants.length - 1 ? ';' : ''}`);
    }
    lines.push('');
  }
  return lines;
}

function renderMethodTypeDeclarations(method: MethodIR): string[] {
  const lines: string[] = [];
  const base = operationTypeBase(method.operationId);
  if (method.params?.length) lines.push(...renderInterface(`${base}Params`, method.params), '');
  if (method.query?.length) lines.push(...renderInterface(`${base}Query`, method.query), '');
  if (method.body?.length) lines.push(...renderInterface(`${base}Body`, method.body), '');
  if (method.response?.length) lines.push(...renderInterface(`${base}Response`, method.response), '');
  return lines;
}

function renderInterface(name: string, fields: FieldIR[]): string[] {
  const lines: string[] = [];
  lines.push(`export interface ${name} {`);
  for (const field of fields) {
    const opt = field.optional ? '?' : '';
    const type = field.array ? `${field.tsType}[]` : field.tsType;
    lines.push(`  ${field.name}${opt}: ${type};`);
  }
  lines.push('}');
  return lines;
}

// ---------------------------------------------------------------------------
// Client file
// ---------------------------------------------------------------------------

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one deterministic renderer owns the complete client file
function generateClientFile(service: ServiceIR, spec: SpecIR, options: TsGeneratorOptions): string {
  const hasStreaming = service.methods.some((m) => m.streaming);
  const lines: string[] = [
    '// Auto-generated by @putnami/client — do not edit',
    '',
    options.design
      ? `import { BaseClient, type ClientConfig, type GeneratedClientDesign } from '@putnami/client';`
      : `import { BaseClient, type ClientConfig } from '@putnami/client';`,
  ];

  if (hasStreaming) {
    lines.push(`import type { StreamObserver } from '@putnami/client';`);
  }

  // Collect type imports
  const typeImports = collectTypeImports(service);
  if (typeImports.length > 0) {
    lines.push(`import type { ${typeImports.join(', ')} } from './types';`);
  }

  lines.push('');

  // Emit specHash and protoMeta as static properties
  if (spec.specHash) {
    lines.push(`/** Spec hash at generation time — used for drift detection */`);
    lines.push(`const SPEC_HASH = '${sanitizeStringLiteral(spec.specHash)}';`);
    lines.push('');
  }

  if (spec.protoMeta) {
    lines.push(`/** Proto field metadata — embedded for zero-config binary encoding */`);
    lines.push(`const PROTO_META = ${JSON.stringify(spec.protoMeta)} as const;`);
    lines.push('');
  }

  lines.push(`export class ${service.className} extends BaseClient {`);
  lines.push(`  readonly serviceName = '${sanitizeStringLiteral(toKebabCase(service.name.replace(/Service$/, '')))}';`);

  if (spec.specHash) {
    lines.push(`  static readonly specHash = SPEC_HASH;`);
  }

  if (spec.packageName) {
    lines.push(`  static readonly packageName = '${sanitizeStringLiteral(spec.packageName)}';`);
  }

  if (options.design) {
    const producers = options.design.operations ?? [];
    const design = {
      language: 'ts',
      client: service.className,
      service: service.name,
      ...(spec.specHash ? { specHash: spec.specHash } : {}),
      operations: service.methods.map((method) => {
        const httpMethod = method.httpMethod.toUpperCase();
        const producer = producers.find(
          (entry) => entry.method.toUpperCase() === httpMethod && entry.path === method.path,
        );
        return {
          // The canonical operation id, never `method.name` — that one is the
          // TypeScript symbol and a normalizer may have rewritten it.
          operationId: method.operationId,
          method: httpMethod,
          path: method.path,
          ...(producer ? { producerProject: producer.producerProject, producerFeature: producer.producerFeature } : {}),
        };
      }),
    };
    lines.push(`  static readonly design = ${JSON.stringify(design)} as const satisfies GeneratedClientDesign;`);
  }

  lines.push('');
  lines.push(`  constructor(config: ClientConfig) {`);

  if (spec.protoMeta) {
    // Auto-inject proto metadata and encoding — zero config for the user
    lines.push(`    super({`);
    lines.push(`      ...config,`);
    lines.push(`      encoding: config.encoding ?? 'proto',`);
    lines.push(
      `      protoMeta: config.protoMeta ?? { messageMeta: PROTO_META.messageMeta, enumTypes: [...PROTO_META.enumTypes] },`,
    );
    if (options.design) {
      lines.push(`      design: ${service.className}.design,`);
    }
    lines.push(`    });`);
  } else if (options.design) {
    lines.push(`    super({ ...config, design: ${service.className}.design });`);
  } else {
    lines.push(`    super(config);`);
  }
  lines.push(`  }`);

  for (const method of service.methods) {
    lines.push('');
    if (method.streaming) {
      lines.push(...generateStreamingMethod(method, spec));
    } else {
      lines.push(...generateMethod(method, spec, options));
    }
  }

  lines.push('}');
  lines.push('');

  return lines.join('\n');
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: method rendering keeps wire-shape branches explicit
function generateMethod(method: MethodIR, spec: SpecIR, options: TsGeneratorOptions): string[] {
  const lines: string[] = [];
  const args = buildMethodArgs(method);
  const returnType = buildReturnType(method);

  // The canonical operation id is what the runtime resolves the feature trace
  // with, so it travels with the call rather than being reverse-matched from the
  // URL. `method.name` is the TypeScript symbol and may have been normalized;
  // `method.operationId` is the contract's own identity. Emitted only when the
  // client carries a descriptor to resolve against.
  const operationIdPart = options.design
    ? `      operationId: '${sanitizeStringLiteral(method.operationId)}',`
    : undefined;

  lines.push(`  async ${method.name}(${args}): Promise<${returnType}> {`);

  if (spec.transport === 'connect') {
    // Connect transport: send body directly
    const bodyExpr = method.body ? 'body' : undefined;
    lines.push(`    return this.request('POST', '${sanitizeStringLiteral(method.path)}', {`);
    if (bodyExpr) {
      lines.push(`      body: ${bodyExpr},`);
    }
    if (operationIdPart) {
      lines.push(operationIdPart);
    }
    lines.push(`    });`);
  } else {
    // HTTP transport: separate params, query, body
    const requestParts: string[] = [];
    if (method.params && method.params.length > 0) {
      requestParts.push(`      params: ${buildParamsExpression(method.params)},`);
    }
    if (method.query && method.query.length > 0) {
      requestParts.push(`      query: ${buildQueryExpression(method.query)},`);
    }
    if (hasRequestBody(method)) {
      requestParts.push(`      body,`);
    }
    if (operationIdPart) {
      requestParts.push(operationIdPart);
    }

    if (requestParts.length > 0) {
      lines.push(
        `    return this.request('${sanitizeStringLiteral(method.httpMethod)}', '${sanitizeStringLiteral(method.path)}', {`,
      );
      lines.push(...requestParts);
      lines.push(`    });`);
    } else {
      lines.push(
        `    return this.request('${sanitizeStringLiteral(method.httpMethod)}', '${sanitizeStringLiteral(method.path)}');`,
      );
    }
  }

  lines.push(`  }`);
  return lines;
}

function generateStreamingMethod(method: MethodIR, _spec: SpecIR): string[] {
  const lines: string[] = [];
  const args = buildMethodArgs(method);
  const responseType = buildReturnType(method);

  if (method.streaming === 'server') {
    // Server-streaming: returns an observer the caller subscribes to
    lines.push(`  ${method.name}(${args}): StreamObserver<${responseType}> {`);
    lines.push(`    return this.stream('${sanitizeStringLiteral(method.path)}', {`);
    if (method.body) {
      lines.push(`      body,`);
    }
    lines.push(`    });`);
  } else if (method.streaming === 'client' || method.streaming === 'bidirectional') {
    // Client or bidi streaming: returns a duplex stream
    const inputType = method.body ? `${operationTypeBase(method.operationId)}Body` : 'unknown';
    lines.push(
      `  ${method.name}(${args}): StreamObserver<${responseType}> & { send(data: ${inputType}): void; end(): void } {`,
    );
    lines.push(`    return this.streamDuplex('${sanitizeStringLiteral(method.path)}', {`);
    if (method.body) {
      lines.push(`      body,`);
    }
    lines.push(`    });`);
  }

  lines.push(`  }`);
  return lines;
}

function buildMethodArgs(method: MethodIR): string {
  const args: string[] = [];

  if (method.params && method.params.length > 0) {
    const typeName = `${operationTypeBase(method.operationId)}Params`;
    args.push(`params: ${typeName}`);
  }

  const bodyTypeName = methodBodyType(method);
  if (bodyTypeName) {
    args.push(`body: ${bodyTypeName}`);
  }

  if (method.query && method.query.length > 0) {
    const typeName = `${operationTypeBase(method.operationId)}Query`;
    const allOptional = method.query.every((f) => f.optional);
    args.push(`query${allOptional ? '?' : ''}: ${typeName}`);
  }

  return args.join(', ');
}

function buildReturnType(method: MethodIR): string {
  if (method.responseType) {
    return method.responseType;
  }
  if (method.response && method.response.length > 0) {
    return `${operationTypeBase(method.operationId)}Response`;
  }
  return 'void';
}

/**
 * The TS type of the request body, or undefined when the method has none.
 * A named `bodyType` ($ref) takes precedence over the per-operation `…Body`
 * interface synthesized from inline fields.
 */
function methodBodyType(method: MethodIR): string | undefined {
  if (method.bodyType) return method.bodyType;
  if (method.body && method.body.length > 0) return `${operationTypeBase(method.operationId)}Body`;
  return undefined;
}

/** Whether the method sends a request body (inline fields or a named type). */
function hasRequestBody(method: MethodIR): boolean {
  return methodBodyType(method) !== undefined;
}

function buildParamsExpression(params: FieldIR[]): string {
  // Convert typed params object to Record<string, string> for path substitution
  if (params.length === 1) {
    return `{ ${params[0].name}: String(params.${params[0].name}) }`;
  }
  const entries = params.map((p) => `${p.name}: String(params.${p.name})`).join(', ');
  return `{ ${entries} }`;
}

function buildQueryExpression(query: FieldIR[]): string {
  if (query.length === 1) {
    const q = query[0];
    return `{ ${q.name}: query${q.optional ? '?' : ''}.${q.name} != null ? String(query${q.optional ? '?' : ''}.${q.name}) : '' }`;
  }
  // For multiple query params, build inline object
  const entries = query.map((q) => {
    const access = `query${q.optional ? '?.' : '.'}${q.name}`;
    return `${q.name}: ${access} != null ? String(${access}) : ''`;
  });
  return `{ ${entries.join(', ')} }`;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: import collection covers each generated type category
function collectTypeImports(service: ServiceIR): string[] {
  // A Set dedupes shared named types referenced by more than one method (a $ref
  // body/response type appears once in the import even when reused).
  const imports = new Set<string>();
  for (const method of service.methods) {
    const base = operationTypeBase(method.operationId);
    if (method.params && method.params.length > 0) imports.add(`${base}Params`);
    if (method.query && method.query.length > 0) imports.add(`${base}Query`);
    if (method.bodyType) imports.add(method.bodyType);
    else if (method.body && method.body.length > 0) imports.add(`${base}Body`);
    if (method.responseType) imports.add(method.responseType);
    else if (method.response && method.response.length > 0) imports.add(`${base}Response`);
  }
  return [...imports];
}

// ---------------------------------------------------------------------------
// Index file
// ---------------------------------------------------------------------------

function generateIndexFile(spec: SpecIR): string {
  const lines: string[] = ['// Auto-generated by @putnami/client — do not edit', '', `export * from './types';`];

  for (const service of spec.services) {
    lines.push(`export { ${service.className} } from './${toKebabCase(service.className)}';`);
  }

  lines.push('');
  return lines.join('\n');
}

// ---------------------------------------------------------------------------
// Package.json
// ---------------------------------------------------------------------------

function generatePackageJson(options: TsGeneratorOptions): string {
  const pkg = {
    name: options.packageName,
    version: options.version ?? '0.0.1',
    description: options.description ?? `Generated API client for ${options.packageName}`,
    main: 'src/index.ts',
    exports: { '.': './src/index.ts' },
    dependencies: {
      '@putnami/client': options.clientDependency ?? 'workspace:*',
    },
    private: false,
  };
  return `${JSON.stringify(pkg, null, 2)}\n`;
}

// ---------------------------------------------------------------------------
// String utilities
// ---------------------------------------------------------------------------

/**
 * Escape a value for safe embedding in a single-quoted TypeScript string literal.
 * Prevents code injection from spec-derived values (service names, paths, etc.).
 */
function sanitizeStringLiteral(value: string): string {
  return value
    .replace(/\\/g, '\\\\')
    .replace(/'/g, "\\'")
    .replace(/[\r\n]/g, '');
}
