import type { ClientExactNumber, ClientSchema } from '@putnami/application';
import { ClientRequestEncodingError, ClientResponseContractError } from './errors';

class RawNumber {
  constructor(readonly value: string) {}
}

/**
 * The lexeme of a number {@link parseJsonValue} read without interpreting it,
 * or undefined for any other value. The response cache reads an integer's
 * digits from it without rounding them through a double.
 */
export function rawJsonNumberLexeme(value: unknown): string | undefined {
  return value instanceof RawNumber ? value.value : undefined;
}

type SchemaGraph = Readonly<Record<string, ClientSchema>>;

/** Encode one OpenAPI path/query/header value without losing bigint or bytes. */
export function encodeHttpParameter(
  value: unknown,
  schema: ClientSchema,
  options: { location: 'query' },
): string | string[] | undefined;
export function encodeHttpParameter(
  value: unknown,
  schema: ClientSchema,
  options?: { location: 'path' | 'header' },
): string | undefined;
export function encodeHttpParameter(
  value: unknown,
  schema: ClientSchema,
  options?: { location: 'path' | 'query' | 'header' },
): string | string[] | undefined {
  if (value === undefined) return undefined;
  if (value === null) {
    if (schema.nullable) return 'null';
    throw new ClientRequestEncodingError('non-nullable parameter is null');
  }
  if (Array.isArray(value)) {
    if (schema.type !== 'array' || !schema.items)
      throw new ClientRequestEncodingError('parameter array has no item schema');
    const itemSchema = schema.items;
    const items = value.map((item) => encodeHttpParameter(item, itemSchema) ?? '');
    return options?.location === 'query' ? items : items.join(',');
  }
  if (typeof value === 'bigint') return value.toString(10);
  if (value instanceof Uint8Array) return encodeBase64(value);
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return String(value);
  throw new ClientRequestEncodingError('parameter value is not representable on the declared wire');
}

/** Schema-directed JSON encoder with exact int64/uint64 and byte handling. */
export function encodeJsonBody(value: unknown, schema: ClientSchema, schemas: SchemaGraph = {}): string {
  try {
    return encodeValue(value, schema, schemas, new Set());
  } catch (error) {
    if (error instanceof ClientRequestEncodingError) throw error;
    throw new ClientRequestEncodingError('request body does not match its generated schema');
  }
}

/**
 * Schema-directed JSON decoder that never routes wide integers through Number.
 *
 * It reads documents the provider sent. A property that a closed object
 * (`additionalProperties: false`) does not declare is dropped from the result
 * rather than refused, so a provider can add an optional response property
 * before every client is regenerated. Every declared property is validated.
 */
export function decodeJsonBody(source: string, schema: ClientSchema, schemas: SchemaGraph = {}): unknown {
  let raw: unknown;
  try {
    raw = parseJsonValue(source);
  } catch {
    throw new ClientResponseContractError('response body is not valid JSON');
  }
  try {
    return decodeValue(raw, schema, schemas, new Set(), true);
  } catch (error) {
    if (error instanceof ClientResponseContractError) throw error;
    throw new ClientResponseContractError('response body does not match its generated schema');
  }
}

/** Parse untrusted JSON while retaining numeric lexemes and rejecting duplicate keys. */
export function parseJsonValue(source: string): unknown {
  return new JsonParser(source).parse();
}

/**
 * Validate/project an already parsed value the provider sent through the same
 * schema codec, with the same rule as {@link decodeJsonBody}: a property a
 * closed object does not declare is dropped.
 */
export function decodeJsonValue(value: unknown, schema: ClientSchema, schemas: SchemaGraph = {}): unknown {
  return decodeValue(value, schema, schemas, new Set(), true);
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: schema-directed encoding distinguishes every supported scalar and composite form without lossy fallback
function encodeValue(value: unknown, schema: ClientSchema, schemas: SchemaGraph, refs: Set<string>): string {
  const resolved = resolveSchema(schema, schemas, refs, ClientRequestEncodingError);
  // Null is one of the values an opaque declaration admits, so it is checked
  // before the nullable rule.
  if (isOpaqueJson(resolved)) return encodeOpaqueJson(value, new Set());
  if (value === null) {
    if (!resolved.nullable) throw new ClientRequestEncodingError('non-nullable body value is null');
    return 'null';
  }
  if (resolved.oneOf) {
    return selectUnion(
      value,
      resolved,
      (branch) => encodeValue(value, branch, schemas, refs),
      ClientRequestEncodingError,
    );
  }
  validateEnum(value, resolved.enum, ClientRequestEncodingError);
  switch (resolved.type) {
    case 'string':
      if (resolved.format === 'byte' || resolved.format === 'binary') {
        if (!(value instanceof Uint8Array))
          throw new ClientRequestEncodingError('binary body value must be Uint8Array');
        return JSON.stringify(encodeBase64(value));
      }
      if (typeof value !== 'string') throw new ClientRequestEncodingError('body value must be string');
      validateString(value, resolved, ClientRequestEncodingError);
      return JSON.stringify(value);
    case 'boolean':
      if (typeof value !== 'boolean') throw new ClientRequestEncodingError('body value must be boolean');
      return String(value);
    case 'number':
      if (typeof value !== 'number' || !Number.isFinite(value))
        throw new ClientRequestEncodingError('body value must be finite number');
      validateNumberBounds(value, resolved, ClientRequestEncodingError);
      return String(value);
    case 'integer':
      return encodeInteger(value, resolved);
    case 'array': {
      if (!Array.isArray(value) || !resolved.items) throw new ClientRequestEncodingError('body value must be array');
      const items = resolved.items;
      validateArray(value, resolved, ClientRequestEncodingError);
      const encoded = value.map((entry) => encodeValue(entry, items, schemas, refs));
      if (resolved.uniqueItems && new Set(encoded).size !== encoded.length) {
        throw new ClientRequestEncodingError('body array values must be unique');
      }
      return `[${encoded.join(',')}]`;
    }
    case 'object':
      return encodeObject(value, resolved, schemas, refs);
    default:
      throw new ClientRequestEncodingError('body schema is not representable');
  }
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: closed-object encoding must enforce required, declared, and typed additional fields together
function encodeObject(value: unknown, schema: ClientSchema, schemas: SchemaGraph, refs: Set<string>): string {
  if (!isRecord(value)) throw new ClientRequestEncodingError('body value must be object');
  const properties = schema.properties ?? {};
  const required = new Set(schema.required ?? []);
  const entries: string[] = [];
  for (const [name, childSchema] of Object.entries(properties)) {
    const child = Object.hasOwn(value, name) ? value[name] : undefined;
    if (child === undefined) {
      if (required.has(name)) throw new ClientRequestEncodingError(`body is missing required field ${name}`);
      continue;
    }
    entries.push(`${JSON.stringify(name)}:${encodeValue(child, childSchema, schemas, refs)}`);
  }
  for (const [name, child] of Object.entries(value)) {
    if (Object.hasOwn(properties, name)) continue;
    if (typeof schema.additionalProperties === 'object') {
      entries.push(`${JSON.stringify(name)}:${encodeValue(child, schema.additionalProperties, schemas, refs)}`);
    } else if (schema.additionalProperties === true) {
      // A free-form member is an opaque JSON value; an undefined one is absent.
      if (child !== undefined) entries.push(`${JSON.stringify(name)}:${encodeOpaqueJson(child, new Set())}`);
    } else if (schema.additionalProperties === false) {
      throw new ClientRequestEncodingError(`body contains undeclared field ${name}`);
    }
  }
  return `{${entries.join(',')}}`;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: schema-directed decoding distinguishes every supported scalar and composite form without lossy fallback
function decodeValue(
  value: unknown,
  schema: ClientSchema,
  schemas: SchemaGraph,
  refs: Set<string>,
  dropUndeclared: boolean,
): unknown {
  const resolved = resolveSchema(schema, schemas, refs, ClientResponseContractError);
  if (isOpaqueJson(resolved)) return decodeOpaqueJson(value);
  if (value === null) {
    if (!resolved.nullable) throw new ClientResponseContractError('non-nullable response value is null');
    return null;
  }
  if (resolved.oneOf) {
    // A value that matches a variant as sent selects exactly as the strict
    // rule does. Only a value no variant matches as sent is decoded again with
    // undeclared properties dropped, and it must then match exactly one variant.
    let matches = unionMatches(
      value,
      resolved,
      (branch) => decodeValue(value, branch, schemas, refs, false),
      ClientResponseContractError,
    );
    if (matches.length === 0 && dropUndeclared) {
      matches = unionMatches(
        value,
        resolved,
        (branch) => decodeValue(value, branch, schemas, refs, true),
        ClientResponseContractError,
      );
    }
    return onlyUnionMatch(matches, ClientResponseContractError);
  }
  let decoded: unknown;
  switch (resolved.type) {
    case 'string':
      // A protobuf reply carries a bytes field already decoded; base64 is its
      // JSON form only.
      if ((resolved.format === 'byte' || resolved.format === 'binary') && value instanceof Uint8Array) {
        decoded = value;
        break;
      }
      if (typeof value !== 'string') throw new ClientResponseContractError('response value must be string');
      decoded = resolved.format === 'byte' || resolved.format === 'binary' ? decodeBase64(value) : value;
      if (typeof decoded === 'string') validateString(decoded, resolved, ClientResponseContractError);
      break;
    case 'boolean':
      if (typeof value !== 'boolean') throw new ClientResponseContractError('response value must be boolean');
      decoded = value;
      break;
    case 'number': {
      const number = decodeNumber(value);
      validateNumberBounds(number, resolved, ClientResponseContractError);
      decoded = number;
      break;
    }
    case 'integer':
      decoded = decodeInteger(value, resolved);
      break;
    case 'array': {
      if (!Array.isArray(value) || !resolved.items)
        throw new ClientResponseContractError('response value must be array');
      const items = resolved.items;
      validateArray(value, resolved, ClientResponseContractError);
      decoded = value.map((entry) => decodeValue(entry, items, schemas, refs, dropUndeclared));
      if (resolved.uniqueItems && new Set(value.map(stableValueKey)).size !== value.length) {
        throw new ClientResponseContractError('response array values must be unique');
      }
      break;
    }
    case 'object':
      decoded = decodeObject(value, resolved, schemas, refs, dropUndeclared);
      break;
    default:
      throw new ClientResponseContractError('response schema is not representable');
  }
  validateEnum(decoded, resolved.enum, ClientResponseContractError);
  return decoded;
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: closed-object decoding validates required, declared, and typed additional fields together
function decodeObject(
  value: unknown,
  schema: ClientSchema,
  schemas: SchemaGraph,
  refs: Set<string>,
  dropUndeclared: boolean,
): unknown {
  if (!isRecord(value)) throw new ClientResponseContractError('response value must be object');
  const result: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
  const properties = schema.properties ?? {};
  const required = new Set(schema.required ?? []);
  for (const [name, childSchema] of Object.entries(properties)) {
    if (!Object.hasOwn(value, name)) {
      if (required.has(name)) throw new ClientResponseContractError(`response is missing required field ${name}`);
      continue;
    }
    result[name] = decodeValue(value[name], childSchema, schemas, refs, dropUndeclared);
  }
  for (const [name, child] of Object.entries(value)) {
    if (Object.hasOwn(properties, name)) continue;
    if (typeof schema.additionalProperties === 'object') {
      result[name] = decodeValue(child, schema.additionalProperties, schemas, refs, dropUndeclared);
    } else if (schema.additionalProperties === true) {
      result[name] = decodeOpaqueJson(child);
    } else if (schema.additionalProperties === false && !dropUndeclared) {
      throw new ClientResponseContractError('response contains an undeclared field');
    }
  }
  return { ...result };
}

/** Whether a schema declares an opaque JSON value (`x-putnami-json: any`). */
function isOpaqueJson(schema: ClientSchema): boolean {
  return schema['x-putnami-json'] !== undefined;
}

/**
 * Write an opaque JSON value. It accepts exactly the JSON value model the
 * decoder produces — null, booleans, strings, finite numbers, bigints, arrays
 * and plain objects — and refuses anything else rather than letting a
 * `toJSON`, a Date or a Map pick an encoding the other runtime never reads.
 * A bigint is written as its exact digits, so an integer outside the safe
 * range survives the round trip.
 */
// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one closed switch over the JSON value model
function encodeOpaqueJson(value: unknown, ancestors: Set<object>): string {
  if (value === null) return 'null';
  switch (typeof value) {
    case 'boolean':
      return String(value);
    case 'string':
      return JSON.stringify(value);
    case 'bigint':
      return value.toString(10);
    case 'number':
      if (!Number.isFinite(value)) throw new ClientRequestEncodingError('opaque JSON number must be finite');
      return JSON.stringify(value);
    case 'object': {
      if (ancestors.has(value)) throw new ClientRequestEncodingError('opaque JSON value is cyclic');
      const prototype = Object.getPrototypeOf(value);
      if (!Array.isArray(value) && prototype !== Object.prototype && prototype !== null) {
        throw new ClientRequestEncodingError('opaque JSON value must be a plain object, an array or a scalar');
      }
      const next = new Set(ancestors).add(value);
      if (Array.isArray(value)) {
        return `[${value
          .map((entry) => {
            if (entry === undefined) throw new ClientRequestEncodingError('opaque JSON array holds undefined');
            return encodeOpaqueJson(entry, next);
          })
          .join(',')}]`;
      }
      const entries: string[] = [];
      for (const [name, entry] of Object.entries(value)) {
        if (entry !== undefined) entries.push(`${JSON.stringify(name)}:${encodeOpaqueJson(entry, next)}`);
      }
      return `{${entries.join(',')}}`;
    }
    default:
      throw new ClientRequestEncodingError('opaque JSON value is not a JSON value');
  }
}

/**
 * Read an opaque JSON value into plain JSON values. An integer lexeme outside
 * the safe range becomes a bigint so no integer is rounded; any other number
 * is an IEEE-754 double, as JSON.parse reads it, and one outside the double
 * range is refused rather than read as Infinity.
 */
function decodeOpaqueJson(value: unknown): unknown {
  if (value === null || typeof value === 'boolean' || typeof value === 'string') return value;
  if (value instanceof RawNumber) {
    if (/^-?(0|[1-9]\d*)$/.test(value.value)) {
      const number = Number(value.value);
      return Number.isSafeInteger(number) ? number : BigInt(value.value);
    }
    const number = Number(value.value);
    if (!Number.isFinite(number)) throw new ClientResponseContractError('opaque JSON number exceeds the double range');
    return number;
  }
  if (typeof value === 'number' || typeof value === 'bigint') return value;
  if (Array.isArray(value)) return value.map(decodeOpaqueJson);
  if (isRecord(value)) {
    const result: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
    for (const [name, entry] of Object.entries(value)) result[name] = decodeOpaqueJson(entry);
    return { ...result };
  }
  throw new ClientResponseContractError('opaque JSON value is not a JSON value');
}

function resolveSchema<E extends Error>(
  schema: ClientSchema,
  schemas: SchemaGraph,
  refs: Set<string>,
  ErrorClass: new (message: string) => E,
): ClientSchema {
  if (!schema.$ref) return schema;
  const prefix = '#/components/schemas/';
  if (!schema.$ref.startsWith(prefix) || refs.has(schema.$ref)) throw new ErrorClass('schema reference is invalid');
  const resolved = schemas[schema.$ref.slice(prefix.length)];
  if (!resolved) throw new ErrorClass('schema reference is missing');
  const next = new Set(refs);
  next.add(schema.$ref);
  const target = resolveSchema(resolved, schemas, next, ErrorClass);
  return schema.nullable === undefined ? target : { ...target, nullable: schema.nullable };
}

function encodeInteger(value: unknown, schema: ClientSchema): string {
  const format = schema.format;
  if (format === 'int64' || format === 'uint64') {
    if (typeof value !== 'bigint') throw new ClientRequestEncodingError('wide integer body value must be bigint');
    assertIntegerBounds(value, format, ClientRequestEncodingError);
    validateIntegerBoundsPolicy(value, schema, ClientRequestEncodingError);
    return value.toString(10);
  }
  if (typeof value !== 'number' || !Number.isSafeInteger(value)) {
    throw new ClientRequestEncodingError('integer body value must be a safe integer');
  }
  assertIntegerBounds(BigInt(value), format, ClientRequestEncodingError);
  validateIntegerBoundsPolicy(BigInt(value), schema, ClientRequestEncodingError);
  return String(value);
}

function decodeInteger(value: unknown, schema: ClientSchema): number | bigint {
  const format = schema.format;
  // A protobuf reply carries a 64-bit field as a bigint; JSON carries it as the
  // raw number text.
  const text =
    value instanceof RawNumber
      ? value.value
      : typeof value === 'bigint'
        ? value.toString()
        : typeof value === 'number' && Number.isSafeInteger(value)
          ? String(value)
          : '';
  if (!/^-?(0|[1-9]\d*)$/.test(text)) throw new ClientResponseContractError('response integer is invalid');
  const integer = BigInt(text);
  assertIntegerBounds(integer, format, ClientResponseContractError);
  validateIntegerBoundsPolicy(integer, schema, ClientResponseContractError);
  if (format === 'int64' || format === 'uint64') return integer;
  const number = Number(integer);
  if (!Number.isSafeInteger(number)) throw new ClientResponseContractError('response integer exceeds safe range');
  return number;
}

function assertIntegerBounds<E extends Error>(
  value: bigint,
  format: ClientSchema['format'],
  ErrorClass: new (message: string) => E,
): void {
  const bounds =
    format === 'uint64'
      ? [0n, 18_446_744_073_709_551_615n]
      : format === 'int64'
        ? [-9_223_372_036_854_775_808n, 9_223_372_036_854_775_807n]
        : format === 'uint32'
          ? [0n, 4_294_967_295n]
          : format === 'int32'
            ? [-2_147_483_648n, 2_147_483_647n]
            : undefined;
  if (bounds && (value < bounds[0] || value > bounds[1])) throw new ErrorClass('integer is outside its declared range');
}

function decodeNumber(value: unknown): number {
  const number = value instanceof RawNumber ? Number(value.value) : typeof value === 'number' ? value : NaN;
  if (!Number.isFinite(number)) throw new ClientResponseContractError('response number is invalid');
  return number;
}

function selectUnion<T, E extends Error>(
  value: unknown,
  schema: ClientSchema,
  project: (branch: ClientSchema) => T,
  ErrorClass: new (message: string) => E,
): T {
  return onlyUnionMatch(unionMatches(value, schema, project, ErrorClass), ErrorClass);
}

/** The projections of every variant the value matches, after the discriminator narrowed them. */
function unionMatches<T, E extends Error>(
  value: unknown,
  schema: ClientSchema,
  project: (branch: ClientSchema) => T,
  ErrorClass: new (message: string) => E,
): T[] {
  let branches = [...(schema.oneOf ?? [])];
  const discriminator = schema.discriminator;
  if (discriminator) {
    if (!isRecord(value) || typeof value[discriminator.propertyName] !== 'string') {
      throw new ErrorClass('union discriminator is missing');
    }
    const target = discriminator.mapping?.[value[discriminator.propertyName] as string];
    if (target) branches = branches.filter((branch) => branch.$ref === target);
    if (target && branches.length !== 1) throw new ErrorClass('union discriminator mapping is invalid');
  }
  const matches: T[] = [];
  for (const branch of branches) {
    try {
      matches.push(project(branch));
    } catch {}
  }
  return matches;
}

/** A union value selects exactly one variant. */
function onlyUnionMatch<T, E extends Error>(matches: T[], ErrorClass: new (message: string) => E): T {
  if (matches.length !== 1) {
    throw new ErrorClass(
      matches.length === 0 ? 'value matches no declared union variant' : 'value matches multiple union variants',
    );
  }
  return matches[0] as T;
}

function validateString<E extends Error>(
  value: string,
  schema: ClientSchema,
  ErrorClass: new (message: string) => E,
): void {
  const length = [...value].length;
  if (schema.minLength !== undefined && length < schema.minLength)
    throw new ErrorClass('string is shorter than minLength');
  if (schema.maxLength !== undefined && length > schema.maxLength)
    throw new ErrorClass('string is longer than maxLength');
  if (schema.pattern !== undefined) {
    let pattern: RegExp;
    try {
      pattern = new RegExp(schema.pattern, 'u');
    } catch {
      throw new ErrorClass('schema pattern is invalid');
    }
    if (!pattern.test(value)) throw new ErrorClass('string does not match pattern');
  }
}

function validateArray<E extends Error>(
  value: readonly unknown[],
  schema: ClientSchema,
  ErrorClass: new (message: string) => E,
): void {
  if (schema.minItems !== undefined && value.length < schema.minItems)
    throw new ErrorClass('array is shorter than minItems');
  if (schema.maxItems !== undefined && value.length > schema.maxItems)
    throw new ErrorClass('array is longer than maxItems');
}

function validateNumberBounds<E extends Error>(
  value: number,
  schema: ClientSchema,
  ErrorClass: new (message: string) => E,
): void {
  const minimum = schema.minimum === undefined ? undefined : Number(exactNumberText(schema.minimum));
  const maximum = schema.maximum === undefined ? undefined : Number(exactNumberText(schema.maximum));
  if (minimum !== undefined && (schema.exclusiveMinimum ? value <= minimum : value < minimum)) {
    throw new ErrorClass('number is below its declared minimum');
  }
  if (maximum !== undefined && (schema.exclusiveMaximum ? value >= maximum : value > maximum)) {
    throw new ErrorClass('number is above its declared maximum');
  }
}

function validateIntegerBoundsPolicy<E extends Error>(
  value: bigint,
  schema: ClientSchema,
  ErrorClass: new (message: string) => E,
): void {
  const minimum = schema.minimum === undefined ? undefined : BigInt(exactNumberText(schema.minimum));
  const maximum = schema.maximum === undefined ? undefined : BigInt(exactNumberText(schema.maximum));
  if (minimum !== undefined && (schema.exclusiveMinimum ? value <= minimum : value < minimum)) {
    throw new ErrorClass('integer is below its declared minimum');
  }
  if (maximum !== undefined && (schema.exclusiveMaximum ? value >= maximum : value > maximum)) {
    throw new ErrorClass('integer is above its declared maximum');
  }
}

function exactNumberText(value: number | ClientExactNumber): string {
  return isExactNumber(value) ? value.$number : String(value);
}

function enumValuesEqual(value: unknown, candidate: string | number | boolean | ClientExactNumber): boolean {
  if (typeof value === 'bigint') {
    const candidateText = isExactNumber(candidate)
      ? candidate.$number
      : typeof candidate === 'number' && Number.isSafeInteger(candidate)
        ? String(candidate)
        : undefined;
    return candidateText !== undefined && /^-?\d+$/.test(candidateText) && value === BigInt(candidateText);
  }
  if (isExactNumber(candidate)) {
    return typeof value === 'number' && Number.isSafeInteger(value) && candidate.$number === String(value);
  }
  return value === candidate;
}

function stableValueKey(value: unknown): string {
  if (value instanceof RawNumber) return `number:${value.value}`;
  if (value instanceof Uint8Array) return `bytes:${encodeBase64(value)}`;
  if (Array.isArray(value)) return `array:[${value.map(stableValueKey).join(',')}]`;
  if (isRecord(value)) {
    return `object:{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${stableValueKey(value[key])}`)
      .join(',')}}`;
  }
  return `${typeof value}:${String(value)}`;
}

function validateEnum<E extends Error>(
  value: unknown,
  values: ClientSchema['enum'],
  ErrorClass: new (message: string) => E,
): void {
  if (!values) return;
  const matches = values.some((candidate) => enumValuesEqual(value, candidate));
  if (!matches) throw new ErrorClass('value is outside the declared enum');
}

function isExactNumber(value: unknown): value is ClientExactNumber {
  return isRecord(value) && typeof value['$number'] === 'string';
}

function encodeBase64(value: Uint8Array): string {
  let binary = '';
  for (const byte of value) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function decodeBase64(value: string): Uint8Array {
  try {
    const binary = atob(value);
    return Uint8Array.from(binary, (character) => character.charCodeAt(0));
  } catch {
    throw new ClientResponseContractError('response byte value is not valid base64');
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) && !(value instanceof RawNumber);
}

/** Small strict parser that retains every numeric source lexeme. */
class JsonParser {
  private index = 0;
  constructor(private readonly source: string) {}

  parse(): unknown {
    const value = this.value();
    this.space();
    if (this.index !== this.source.length) throw new SyntaxError('trailing JSON');
    return value;
  }

  private value(): unknown {
    this.space();
    const char = this.source[this.index];
    if (char === '{') return this.object();
    if (char === '[') return this.array();
    if (char === '"') return this.string();
    if (char === 't' && this.take('true')) return true;
    if (char === 'f' && this.take('false')) return false;
    if (char === 'n' && this.take('null')) return null;
    return this.number();
  }

  private object(): Record<string, unknown> {
    const result: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
    this.index++;
    this.space();
    if (this.source[this.index] === '}') {
      this.index++;
      return result;
    }
    for (;;) {
      const key = this.string();
      this.space();
      if (this.source[this.index++] !== ':') throw new SyntaxError('expected colon');
      if (Object.hasOwn(result, key)) throw new SyntaxError('duplicate JSON key');
      result[key] = this.value();
      this.space();
      const separator = this.source[this.index++];
      if (separator === '}') return result;
      if (separator !== ',') throw new SyntaxError('expected comma');
      this.space();
    }
  }

  private array(): unknown[] {
    const result: unknown[] = [];
    this.index++;
    this.space();
    if (this.source[this.index] === ']') {
      this.index++;
      return result;
    }
    for (;;) {
      result.push(this.value());
      this.space();
      const separator = this.source[this.index++];
      if (separator === ']') return result;
      if (separator !== ',') throw new SyntaxError('expected comma');
    }
  }

  private string(): string {
    if (this.source[this.index++] !== '"') throw new SyntaxError('expected string');
    const start = this.index - 1;
    for (;;) {
      const char = this.source[this.index++];
      if (char === undefined) throw new SyntaxError('unterminated string');
      if (char === '\\') this.index++;
      else if (char === '"') return JSON.parse(this.source.slice(start, this.index)) as string;
      else if (char.charCodeAt(0) < 0x20) throw new SyntaxError('control character');
    }
  }

  private number(): RawNumber {
    const match = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(this.source.slice(this.index));
    if (!match) throw new SyntaxError('invalid JSON value');
    this.index += match[0].length;
    return new RawNumber(match[0]);
  }

  private take(token: string): boolean {
    if (!this.source.startsWith(token, this.index)) return false;
    this.index += token.length;
    return true;
  }

  private space(): void {
    while (/\s/.test(this.source[this.index] ?? '')) this.index++;
  }
}
