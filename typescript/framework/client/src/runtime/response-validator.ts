import type { ProtoFieldMeta } from '@putnami/application';
import { ClientError } from './errors';

/**
 * Thrown when a successful response body does not match the response shape
 * embedded in the generated client.
 *
 * The typed client casts response bodies to the method's declared type. Without
 * a runtime check, a server returning the wrong shape (e.g. `{}` where a `User`
 * is expected) yields a `T`-typed object whose required fields are `undefined`,
 * which downstream code then dereferences as guaranteed. This error surfaces
 * that mismatch at the transport boundary instead.
 *
 * `status` is `0` (no HTTP error occurred — the response was 2xx but malformed).
 * `detail` describes the first violation found (which field, what was wrong).
 */
export class ClientResponseValidationError extends ClientError {
  /** Human-readable description of the first shape violation found. */
  readonly detail: string;

  constructor(options: { service: string; method: string; detail: string; responseBody?: unknown }) {
    super({
      service: options.service,
      method: options.method,
      status: 0,
      message: `Response validation failed for ${options.method}: ${options.detail}`,
      responseBody: options.responseBody,
    });
    this.name = 'ClientResponseValidationError';
    this.detail = options.detail;
  }
}

/**
 * The snake_case → camelCase mapping the proto codec uses when reading fields
 * (`data[field.name] ?? data[snakeToCamel(field.name)]`). Mirrored here so the
 * validator accepts a field under either casing — the Connect JSON path forwards
 * the handler's object as-is, which may use camelCase keys, while binary proto
 * decoding produces snake_case keys.
 */
function snakeToCamel(name: string): string {
  return name.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());
}

/** Look up a field value tolerating either snake_case or camelCase keys. */
function readField(obj: Record<string, unknown>, name: string): unknown {
  const direct = obj[name];
  if (direct !== undefined) return direct;
  return obj[snakeToCamel(name)];
}

/**
 * Scalar proto types whose decoded/JSON value must be a JS `string`.
 * Only `string` is asserted; enum types decode to numbers and are left untyped.
 */
const STRING_TYPE = 'string';

/** Scalar proto types whose decoded value must be a JS `number`. */
const NUMBER_TYPES: ReadonlySet<string> = new Set([
  'double',
  'float',
  'int32',
  'uint32',
  'sint32',
  'fixed32',
  'sfixed32',
]);

/**
 * 64-bit integer types decode to `number` while inside JS's safe integer range
 * and `bigint` above it. Accept both shapes so valid binary proto responses are
 * not rejected after decoding.
 */
const NUMBER_OR_BIGINT_TYPES: ReadonlySet<string> = new Set(['int64', 'uint64', 'sint64', 'fixed64', 'sfixed64']);

/** Scalar proto type whose decoded value must be a JS `boolean`. */
const BOOL_TYPE = 'bool';

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Describe the JS runtime kind of a value for violation messages.
 */
function describeKind(value: unknown): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  return typeof value;
}

/**
 * Validate that a parsed response value structurally matches the proto message
 * shape described by `fields`.
 *
 * Catches the real failure mode — a server returning a response that is missing
 * required fields or whose fields are the wrong type (e.g. `{}` for a `User`, or
 * a `string` field returned as a number) — while staying tolerant of proto/JSON
 * evolution:
 *
 * - **Extra/unknown keys are allowed** (additive evolution; proto3 ignores them).
 * - **Optional fields may be absent** (`field.optional === true`).
 * - **Repeated and map fields may be absent** (an empty list/map is a valid
 *   proto3 default), but if present they must be the right container type.
 * - Required scalar fields (`field.optional === false`, non-repeated, non-map)
 *   must be present, because their absence is exactly the `undefined`-deref bug.
 * - Present values are type-checked only for the scalar/message kinds the codec
 *   produces deterministically; enums, bytes, and unknown types are not asserted.
 *
 * Throws on the first violation. Callers pass the message/method names so the
 * error identifies which call failed.
 *
 * @param value      The parsed response body.
 * @param fields     Field metadata for the response message (already embedded).
 * @param messageMeta All message metadata, used to recurse into nested messages.
 * @param service    Service/package name for error reporting.
 * @param method     RPC path or method label for error reporting.
 */
export function validateResponseShape(
  value: unknown,
  fields: ProtoFieldMeta[],
  messageMeta: Record<string, ProtoFieldMeta[]> | undefined,
  service: string,
  method: string,
): void {
  const detail = checkMessage(value, fields, messageMeta, '');
  if (detail !== undefined) {
    throw new ClientResponseValidationError({ service, method, detail, responseBody: value });
  }
}

/**
 * Recursively check a single message. Returns a violation description, or
 * `undefined` when the value conforms. `path` is the dotted field path used in
 * messages for nested violations ('' at the top level).
 */
function checkMessage(
  value: unknown,
  fields: ProtoFieldMeta[],
  messageMeta: Record<string, ProtoFieldMeta[]> | undefined,
  path: string,
): string | undefined {
  // A proto message always decodes to an object. A primitive/array/null body
  // where a message with fields is expected is the headline failure mode.
  if (!isPlainObject(value)) {
    const where = path === '' ? 'response' : `field "${path}"`;
    return `expected ${where} to be an object, got ${describeKind(value)}`;
  }

  for (const field of fields) {
    const fieldPath = path === '' ? field.name : `${path}.${field.name}`;
    const present = readField(value, field.name);

    // Absent field: only a violation when it's a required (non-optional) scalar
    // or nested-message field. Repeated/map fields default to empty and are
    // legitimately absent.
    if (present === undefined || present === null) {
      if (!field.optional && !field.repeated && !field.mapKeyType) {
        return `missing required field "${fieldPath}"`;
      }
      continue;
    }

    const violation = checkFieldValue(field, present, messageMeta, fieldPath);
    if (violation !== undefined) return violation;
  }

  return undefined;
}

/**
 * Type-check a single present field value against its declared proto type.
 */
// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: proto scalar and collection kinds retain exact diagnostics
function checkFieldValue(
  field: ProtoFieldMeta,
  present: unknown,
  messageMeta: Record<string, ProtoFieldMeta[]> | undefined,
  fieldPath: string,
): string | undefined {
  // Map field: must be a (non-array) object container. Values are not asserted.
  if (field.mapKeyType) {
    if (!isPlainObject(present)) {
      return `field "${fieldPath}" should be a map object, got ${describeKind(present)}`;
    }
    return undefined;
  }

  // Repeated field: must be an array. Element types are checked for nested
  // messages only (scalars within arrays are left untyped, matching the
  // tolerance applied to scalars elsewhere).
  if (field.repeated) {
    if (!Array.isArray(present)) {
      return `field "${fieldPath}" should be an array, got ${describeKind(present)}`;
    }
    const nestedFields = messageMeta?.[field.type];
    if (nestedFields) {
      for (let i = 0; i < present.length; i++) {
        const elem = present[i];
        if (elem === undefined || elem === null) continue;
        const violation = checkMessage(elem, nestedFields, messageMeta, `${fieldPath}[${i}]`);
        if (violation !== undefined) return violation;
      }
    }
    return undefined;
  }

  // Nested message field: recurse using its own metadata.
  const nestedFields = messageMeta?.[field.type];
  if (nestedFields) {
    return checkMessage(present, nestedFields, messageMeta, fieldPath);
  }

  // Scalar field: assert the JS kind the codec produces for the few types we
  // can check deterministically. Enums (number), bytes (Uint8Array) and any
  // unknown/unmapped type are intentionally not asserted.
  return checkScalar(field.type, present, fieldPath);
}

/**
 * Assert a scalar field value matches its proto type, for the subset of types
 * with a deterministic JS representation. Returns `undefined` for types that
 * are intentionally left untyped.
 */
function checkScalar(type: string, present: unknown, fieldPath: string): string | undefined {
  if (type === STRING_TYPE) {
    if (typeof present !== 'string') {
      return `field "${fieldPath}" should be a string, got ${describeKind(present)}`;
    }
    return undefined;
  }
  if (type === BOOL_TYPE) {
    if (typeof present !== 'boolean') {
      return `field "${fieldPath}" should be a boolean, got ${describeKind(present)}`;
    }
    return undefined;
  }
  if (NUMBER_TYPES.has(type)) {
    if (typeof present !== 'number') {
      return `field "${fieldPath}" should be a number, got ${describeKind(present)}`;
    }
    return undefined;
  }
  if (NUMBER_OR_BIGINT_TYPES.has(type)) {
    if (typeof present !== 'number' && typeof present !== 'bigint') {
      return `field "${fieldPath}" should be a number or bigint, got ${describeKind(present)}`;
    }
    return undefined;
  }
  // Enums, bytes, and unknown types: tolerated (no assertion).
  return undefined;
}
