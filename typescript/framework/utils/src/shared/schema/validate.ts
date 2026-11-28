import {
  baseTypeName,
  isNestedSchema,
  isSchemaDescriptor,
  type SchemaConstraint,
  type SchemaDefinition,
  type SchemaDescriptor,
  type SchemaPrimitive,
} from './schema';

export interface ValidationError {
  field: string;
  message: string;
}

export interface ValidateOptions {
  /** Coerce string values to the target type (useful for params / query) */
  coerce?: boolean;
  /** Label used in error messages (e.g. "params", "query", "body") */
  label?: string;
}

export interface ValidateResult {
  data: Record<string, unknown>;
  errors: ValidationError[];
}

/**
 * Validate a plain object against a SchemaDefinition.
 *
 * - Checks required / optional presence
 * - Applies default values from `Default()` descriptors
 * - Validates base types (string, number, boolean)
 * - Coerces string → number/boolean when `coerce: true` (URL params & query)
 * - Runs schema constraints (uuid, email, integer, …)
 * - Validates arrays when using `ArrayOf()`
 * - Validates nested objects
 *
 * @returns An object with the validated `data` and any `errors` found
 */
export function validateSchema<S extends SchemaDefinition>(
  schema: S,
  value: unknown,
  options: ValidateOptions = {},
): ValidateResult {
  const errors: ValidationError[] = [];
  const label = options.label ?? 'field';
  const input = asInputObject(value);
  const data = validateObjectFields(schema, input, label, options, errors);
  return { data, errors };
}

/**
 * The check of one schema, compiled once: for an input {@link validateSchema}
 * accepts it returns the `data` `validateSchema` returns, and `undefined` for
 * an input `validateSchema` rejects, whose errors `validateSchema` then reports.
 * Like `validateSchema`, it reads a non-object input as an empty object.
 */
export type AcceptedInput = (value: unknown) => Record<string, unknown> | undefined;

/** The base types a compiled check accepts a field of. */
const COMPILED_BASE_TYPES: ReadonlySet<string> = new Set(['string', 'number', 'boolean']);

/** A scalar field of a compiled schema, with what its check needs read once. */
interface ScalarField {
  readonly key: string;
  readonly expectedType: string;
  readonly defaultValue: unknown;
  readonly required: boolean;
  readonly constraints: readonly SchemaConstraint[];
}

/**
 * Compiles the {@link AcceptedInput} of a schema whose fields are all scalars:
 * `String`, `Number`, `Boolean`, or a descriptor of base type `string`,
 * `number` or `boolean` with its constraints, `Optional` and `Default`. A
 * per-request caller then pays one loop over the fields for a valid input,
 * instead of walking the schema and building the field paths again; an input
 * it does not accept goes to `validateSchema`, so the errors stay those
 * `validateSchema` reports. The constraints of such an input run twice, here
 * and in `validateSchema`; constraints are stateless, so both runs agree.
 * `undefined` for a schema with any other field (an array, a map, a nested
 * object, or another base type), which keeps `validateSchema` alone.
 */
export function compileAcceptedInput(
  schema: SchemaDefinition,
  options: ValidateOptions = {},
): AcceptedInput | undefined {
  const fields: ScalarField[] = [];
  for (const [key, prop] of Object.entries(schema)) {
    const descriptor = getDescriptor(prop);
    // The branches validatePrimitive takes before the scalar one.
    if (
      (descriptor?.array && descriptor.items) ||
      (descriptor?.map && descriptor.mapValue) ||
      descriptor?.schema ||
      isNestedSchema(prop)
    ) {
      return undefined;
    }
    // A scalar base type only: `unknown`, `object` or a custom one stays with validateSchema.
    const expectedType = baseTypeName(prop);
    if (!COMPILED_BASE_TYPES.has(expectedType)) {
      return undefined;
    }
    fields.push({
      key,
      expectedType,
      defaultValue: descriptor?.default,
      required: isRequiredField(descriptor),
      constraints: descriptor?.constraints ?? [],
    });
  }
  const coerceStrings = options.coerce === true;
  return (value) => {
    const input = asInputObject(value);
    const data: Record<string, unknown> = {};
    for (const field of fields) {
      if (!acceptField(field, input[field.key], coerceStrings, data)) return undefined;
    }
    return data;
  };
}

/**
 * Checks one scalar field as {@link validateSchema} does and adds its value to
 * `data`; false when `validateSchema` reports an error for it.
 */
function acceptField(field: ScalarField, raw: unknown, coerceStrings: boolean, data: Record<string, unknown>): boolean {
  const value = isMissingValue(raw) ? field.defaultValue : raw;
  if (isMissingValue(value)) return !field.required;
  const checked = coerceStrings && typeof value === 'string' ? coerce(value, field.expectedType) : value;
  if (!checkBaseType(checked, field.expectedType)) return false;
  for (const constraint of field.constraints) {
    if (!constraint.validate(checked)) return false;
  }
  data[field.key] = checked;
  return true;
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

interface ValidationBranchResult {
  handled: boolean;
  value: unknown;
}

const UNHANDLED_BRANCH: ValidationBranchResult = { handled: false, value: undefined };

function asInputObject(value: unknown): Record<string, unknown> {
  return (typeof value === 'object' && value !== null ? value : {}) as Record<string, unknown>;
}

function isMissingValue(value: unknown): value is null | undefined {
  return value === undefined || value === null;
}

function isObjectRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function getDescriptor(prop: SchemaPrimitive): SchemaDescriptor | undefined {
  return isSchemaDescriptor(prop) ? prop : undefined;
}

function resolveInputValue(raw: unknown, descriptor: SchemaDescriptor | undefined): unknown {
  if (isMissingValue(raw) && descriptor?.default !== undefined) {
    return descriptor.default;
  }
  return raw;
}

function isRequiredField(descriptor: SchemaDescriptor | undefined): boolean {
  if (!descriptor) {
    return true;
  }
  return descriptor.optional !== true && descriptor.default === undefined;
}

function validateObjectFields(
  schema: SchemaDefinition,
  input: Record<string, unknown>,
  parentPath: string,
  options: ValidateOptions,
  errors: ValidationError[],
): Record<string, unknown> {
  const result: Record<string, unknown> = {};

  for (const [key, prop] of Object.entries(schema)) {
    const fieldPath = `${parentPath}.${key}`;
    const descriptor = getDescriptor(prop);
    const value = resolveInputValue(input[key], descriptor);

    if (isMissingValue(value)) {
      if (isRequiredField(descriptor)) {
        errors.push({ field: fieldPath, message: `${fieldPath} is required` });
      }
      continue;
    }

    const validated = validatePrimitive(prop, value, fieldPath, options, errors);
    if (validated !== undefined) {
      result[key] = validated;
    }
  }

  return result;
}

function validatePrimitive(
  prop: SchemaPrimitive,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): unknown {
  const descriptor = getDescriptor(prop);
  const collection = validateCollection(descriptor, raw, path, options, errors);

  if (collection.handled) {
    return collection.value;
  }

  // `Desc(text, { ... })` wraps a nested object in a descriptor that carries
  // the description metadata. Recurse into descriptor.schema before falling
  // back to the bare nested-schema branch so wrapped and unwrapped nested
  // objects validate identically.
  if (descriptor?.schema) {
    return validateNestedObject(descriptor.schema as SchemaDefinition, raw, path, options, errors);
  }

  if (isNestedSchema(prop)) {
    return validateNestedObject(prop as SchemaDefinition, raw, path, options, errors);
  }

  return validateScalar(prop, descriptor, raw, path, options, errors);
}

function validateCollection(
  descriptor: SchemaDescriptor | undefined,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): ValidationBranchResult {
  const arrayResult = validateArray(descriptor, raw, path, options, errors);
  if (arrayResult.handled) {
    return arrayResult;
  }
  return validateMap(descriptor, raw, path, options, errors);
}

function validateArray(
  descriptor: SchemaDescriptor | undefined,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): ValidationBranchResult {
  if (!descriptor?.array || !descriptor.items) {
    return UNHANDLED_BRANCH;
  }

  if (!Array.isArray(raw)) {
    errors.push({ field: path, message: `${path} must be an array` });
    return { handled: true, value: undefined };
  }

  const items = raw.map((item, index) =>
    validatePrimitive(descriptor.items as SchemaPrimitive, item, `${path}[${index}]`, options, errors),
  );
  return { handled: true, value: items };
}

function validateMap(
  descriptor: SchemaDescriptor | undefined,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): ValidationBranchResult {
  if (!descriptor?.map || !descriptor.mapValue) {
    return UNHANDLED_BRANCH;
  }

  if (!isObjectRecord(raw)) {
    errors.push({ field: path, message: `${path} must be an object (map)` });
    return { handled: true, value: undefined };
  }

  // Null-prototype object so a user-supplied `__proto__` (or `constructor`) key
  // becomes a plain own entry instead of reassigning the result's prototype.
  const mapResult: Record<string, unknown> = Object.create(null);
  for (const [key, value] of Object.entries(raw)) {
    const validated = validatePrimitive(
      descriptor.mapValue as SchemaPrimitive,
      value,
      `${path}[${key}]`,
      options,
      errors,
    );
    if (validated !== undefined) {
      mapResult[key] = validated;
    }
  }

  return { handled: true, value: mapResult };
}

function validateNestedObject(
  schema: SchemaDefinition,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): Record<string, unknown> | undefined {
  if (!isObjectRecord(raw)) {
    errors.push({ field: path, message: `${path} must be an object` });
    return undefined;
  }

  return validateNested(schema, raw, path, options, errors);
}

function validateScalar(
  prop: SchemaPrimitive,
  descriptor: SchemaDescriptor | undefined,
  raw: unknown,
  path: string,
  options: ValidateOptions,
  errors: ValidationError[],
): unknown {
  const expectedType = baseTypeName(prop);
  const value = options.coerce && typeof raw === 'string' ? coerce(raw, expectedType) : raw;

  if (!checkBaseType(value, expectedType)) {
    errors.push({ field: path, message: `${path} must be of type ${expectedType}` });
    return undefined;
  }

  applyConstraints(descriptor, value, path, errors);
  return value;
}

function applyConstraints(
  descriptor: SchemaDescriptor | undefined,
  value: unknown,
  path: string,
  errors: ValidationError[],
): void {
  if (!descriptor?.constraints) {
    return;
  }

  for (const constraint of descriptor.constraints) {
    if (!constraint.validate(value)) {
      errors.push({ field: path, message: `${path} ${constraint.message}` });
    }
  }
}

function checkBaseType(value: unknown, expected: string): boolean {
  if (expected === 'string') return typeof value === 'string';
  if (expected === 'number') return typeof value === 'number' && !Number.isNaN(value);
  if (expected === 'boolean') return typeof value === 'boolean';
  if (expected === 'array') return Array.isArray(value);
  if (expected === 'map') return typeof value === 'object' && value !== null && !Array.isArray(value);
  return true;
}

function coerce(raw: string, expected: string): unknown {
  if (expected === 'number') {
    const n = Number(raw);
    return Number.isNaN(n) ? raw : n;
  }
  if (expected === 'boolean') {
    if (raw === 'true') return true;
    if (raw === 'false') return false;
    return raw;
  }
  return raw;
}

function validateNested(
  schema: SchemaDefinition,
  raw: unknown,
  parentPath: string,
  options: ValidateOptions,
  errors: ValidationError[],
): Record<string, unknown> {
  return validateObjectFields(schema, raw as Record<string, unknown>, parentPath, options, errors);
}
