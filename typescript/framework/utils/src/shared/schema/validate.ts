import {
  baseTypeName,
  isNestedSchema,
  isSchemaDescriptor,
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
