const SCHEMA_MARKER = 'putnami:schema' as const;

/**
 * Descriptor for a typed, validated schema property.
 * Created by schema helpers like `Optional()`, `ArrayOf()`, `Uuid`, `Email`, etc.
 *
 * JS builtins (`String`, `Number`, `Boolean`) are used directly for simple types.
 */
export interface SchemaDescriptor<T = unknown> {
  readonly __schema: typeof SCHEMA_MARKER;
  readonly baseType: string;
  readonly optional?: boolean;
  readonly array?: boolean;
  readonly items?: SchemaPrimitive;
  /** Map type flag — set by MapOf() */
  readonly map?: boolean;
  /** Map key type — must be a scalar (String, Number, Boolean, Int) */
  readonly mapKey?: SchemaPrimitive;
  /** Map value type — any SchemaPrimitive */
  readonly mapValue?: SchemaPrimitive;
  readonly constraints?: readonly SchemaConstraint[];
  /** Static default value — field is optional in input but always present in output */
  readonly default?: T;
  /** Environment variable name to read the value from */
  readonly env?: string;
  /** Async resolver function called at bootstrap (before useConfig) */
  readonly resolve?: () => Promise<unknown>;
  /** Mark field as sensitive — validation errors redact the value */
  readonly sensitive?: boolean;
  /**
   * Mark a field whose fallback default is unsafe in production (in-memory
   * store, process-generated key, permissive transport). Extracted into the
   * config schema so `putnami doctor` can flag it when unset in production.
   */
  readonly productionUnsafeDefault?: boolean;
  /** Human-readable description for documentation (OpenAPI, etc.) */
  readonly description?: string;
  /**
   * Nested object payload — set by `Desc(description, { ... })` so that
   * description metadata can be attached to a nested schema without
   * losing its shape. The validator and config extractor recurse into
   * `schema` when present and `baseType === 'object'`.
   */
  readonly schema?: NestedSchema;
  /** Phantom type for inference — never use at runtime */
  readonly _t?: T;
}

export interface SchemaConstraint {
  readonly name: string;
  readonly value?: unknown;
  readonly validate: (value: unknown) => boolean;
  readonly message: string;
}

/** A property in a schema definition: a JS constructor, a SchemaDescriptor, or a nested object */
export type SchemaPrimitive = typeof String | typeof Number | typeof Boolean | SchemaDescriptor | NestedSchema;

/** An object whose values are all SchemaPrimitives — used for nested object schemas */
export type NestedSchema = { [key: string]: SchemaPrimitive };

/** An object schema: maps property names to schema primitives (including nested objects) */
export type SchemaDefinition = Record<string, SchemaPrimitive>;

// ---------------------------------------------------------------------------
// Type inference
// ---------------------------------------------------------------------------

/** Infer the TypeScript type from a single schema primitive (including nested objects) */
export type InferPrimitive<P> = P extends typeof String
  ? string
  : P extends typeof Number
    ? number
    : P extends typeof Boolean
      ? boolean
      : P extends SchemaDescriptor<infer T>
        ? T
        : P extends Record<string, SchemaPrimitive>
          ? InferSchema<P>
          : never;

type RequiredKeys<S extends SchemaDefinition> = {
  [K in keyof S]: S[K] extends { optional: true } ? never : K;
}[keyof S];

type OptionalKeys<S extends SchemaDefinition> = {
  [K in keyof S]: S[K] extends { optional: true } ? K : never;
}[keyof S];

/** Flatten an intersection to a clean object type */
type Simplify<T> = { [K in keyof T]: T[K] } & {};

/** Infer the full TypeScript type from a SchemaDefinition */
export type InferSchema<S extends SchemaDefinition> = Simplify<
  { [K in RequiredKeys<S>]: InferPrimitive<S[K]> } & { [K in OptionalKeys<S>]?: InferPrimitive<S[K]> }
>;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Identity helper to define and share schemas with proper typing */
export function schema<S extends SchemaDefinition>(definition: S): S {
  return definition;
}

export function isSchemaDescriptor(value: unknown): value is SchemaDescriptor {
  return typeof value === 'object' && value !== null && (value as SchemaDescriptor).__schema === SCHEMA_MARKER;
}

/** Check if a schema property is a nested object schema (plain object with SchemaPrimitive values) */
export function isNestedSchema(value: unknown): value is NestedSchema {
  if (typeof value !== 'object' || value === null) return false;
  if (isSchemaDescriptor(value)) return false;
  if (value === String || value === Number || value === Boolean) return false;
  // Must be a plain object with at least one key
  const keys = Object.keys(value as Record<string, unknown>);
  return keys.length > 0;
}

export function baseTypeName(type: SchemaPrimitive): string {
  if (type === String) return 'string';
  if (type === Number) return 'number';
  if (type === Boolean) return 'boolean';
  if (isSchemaDescriptor(type)) return type.baseType;
  if (isNestedSchema(type)) return 'object';
  return 'unknown';
}

function descriptor<T>(
  baseType: string,
  opts: Partial<Omit<SchemaDescriptor, '__schema' | 'baseType'>> = {},
): SchemaDescriptor<T> {
  return { __schema: SCHEMA_MARKER, baseType, ...opts } as SchemaDescriptor<T>;
}

// ---------------------------------------------------------------------------
// Schema primitives
// ---------------------------------------------------------------------------

/** Mark a property as optional */
export function Optional<P extends SchemaPrimitive>(type: P): SchemaDescriptor<InferPrimitive<P> | undefined> {
  if (isSchemaDescriptor(type)) {
    return { ...type, optional: true } as unknown as SchemaDescriptor<InferPrimitive<P> | undefined>;
  }
  return descriptor<InferPrimitive<P> | undefined>(baseTypeName(type), { optional: true });
}

/** Array of items */
export function ArrayOf<P extends SchemaPrimitive>(items: P): SchemaDescriptor<InferPrimitive<P>[]> {
  return descriptor<InferPrimitive<P>[]>('array', { array: true, items });
}

/**
 * Map from scalar keys to values.
 *
 * Maps to proto3 `map<K, V>` and TypeScript `Record<string, V>`.
 *
 * Key types are restricted to scalar types: `String`, `Number`, `Boolean`, `Int`.
 * Value types can be any SchemaPrimitive including nested objects.
 *
 * @example
 * ```typescript
 * { prices: MapOf(String, Number) }     // Record<string, number>
 * { settings: MapOf(String, String) }   // Record<string, string>
 * { scores: MapOf(String, Int) }        // Record<string, number>
 * ```
 */
export function MapOf<
  K extends typeof String | typeof Number | typeof Boolean | SchemaDescriptor<string | number | boolean>,
  V extends SchemaPrimitive,
>(keyType: K, valueType: V): SchemaDescriptor<Record<string, InferPrimitive<V>>> {
  return descriptor<Record<string, InferPrimitive<V>>>('map', { map: true, mapKey: keyType, mapValue: valueType });
}

// ---------------------------------------------------------------------------
// Stream combinator
// ---------------------------------------------------------------------------

const STREAM_MARKER = 'putnami:stream' as const;

/** Wraps a SchemaDefinition to mark a body or return value as a stream of messages */
export interface StreamSchema<S extends SchemaDefinition = SchemaDefinition> {
  readonly __stream: typeof STREAM_MARKER;
  readonly schema: S;
}

/** Mark a body or return schema as a stream of messages */
export function Stream<S extends SchemaDefinition>(schema: S): StreamSchema<S> {
  return { __stream: STREAM_MARKER, schema };
}

export function isStreamSchema(value: unknown): value is StreamSchema {
  return typeof value === 'object' && value !== null && (value as StreamSchema).__stream === STREAM_MARKER;
}

// ---------------------------------------------------------------------------
// Constrained types
// ---------------------------------------------------------------------------

/**
 * UUID string of any RFC 4122 version/variant.
 *
 * The validator matches the canonical 8-4-4-4-12 hex shape and deliberately does
 * NOT constrain the version or variant nibble, so any version (v1/v3/v4/v5/v7…)
 * and the nil UUID all validate. Tightening this to v4-only would reject
 * legitimate UUIDs — notably v7 (time-ordered) — so the broad shape is intentional.
 */
export const Uuid: SchemaDescriptor<string> = descriptor<string>('string', {
  constraints: [
    {
      name: 'uuid',
      validate: (v) =>
        typeof v === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(v),
      message: 'must be a valid UUID',
    },
  ],
});

/** Email string */
export const Email: SchemaDescriptor<string> = descriptor<string>('string', {
  constraints: [
    {
      name: 'email',
      validate: (v) => typeof v === 'string' && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v),
      message: 'must be a valid email address',
    },
  ],
});

/** Integer number */
export const Int: SchemaDescriptor<number> = descriptor<number>('number', {
  constraints: [
    {
      name: 'integer',
      validate: (v) => typeof v === 'number' && Number.isInteger(v),
      message: 'must be an integer',
    },
  ],
});

/** URL string */
export const Url: SchemaDescriptor<string> = descriptor<string>('string', {
  constraints: [
    {
      name: 'url',
      validate: (v) => {
        if (typeof v !== 'string') return false;
        try {
          new globalThis.URL(v);
          return true;
        } catch {
          return false;
        }
      },
      message: 'must be a valid URL',
    },
  ],
});

/** ISO 8601 date string (e.g. "2024-01-15" or "2024-01-15T10:30:00Z") */
export const DateIso: SchemaDescriptor<string> = descriptor<string>('string', {
  constraints: [
    {
      name: 'dateIso',
      validate: (v) => {
        if (typeof v !== 'string') return false;
        const d = new Date(v);
        return !Number.isNaN(d.getTime()) && /^\d{4}-\d{2}-\d{2}/.test(v);
      },
      message: 'must be a valid ISO 8601 date string',
    },
  ],
});

/** Minimum numeric value (inclusive) */
export function Min(n: number): SchemaDescriptor<number> {
  return descriptor<number>('number', {
    constraints: [
      {
        name: 'min',
        value: n,
        validate: (v) => typeof v === 'number' && v >= n,
        message: `must be >= ${n}`,
      },
    ],
  });
}

/** Maximum numeric value (inclusive) */
export function Max(n: number): SchemaDescriptor<number> {
  return descriptor<number>('number', {
    constraints: [
      {
        name: 'max',
        value: n,
        validate: (v) => typeof v === 'number' && v <= n,
        message: `must be <= ${n}`,
      },
    ],
  });
}

/** Minimum string length */
export function MinLength(n: number): SchemaDescriptor<string> {
  return descriptor<string>('string', {
    constraints: [
      {
        name: 'minLength',
        value: n,
        validate: (v) => typeof v === 'string' && v.length >= n,
        message: `must have length >= ${n}`,
      },
    ],
  });
}

/** Maximum string length */
export function MaxLength(n: number): SchemaDescriptor<string> {
  return descriptor<string>('string', {
    constraints: [
      {
        name: 'maxLength',
        value: n,
        validate: (v) => typeof v === 'string' && v.length <= n,
        message: `must have length <= ${n}`,
      },
    ],
  });
}

/** String matching a regular expression */
export function Pattern(regex: RegExp): SchemaDescriptor<string> {
  // Strip stateful `g`/`y` flags so `test()` never advances `lastIndex`.
  // A shared descriptor validated across requests must be stateless; a global
  // or sticky RegExp would otherwise accept/reject the same value on alternate
  // calls. Build a copy — never mutate the caller's RegExp.
  const safe = new RegExp(regex.source, regex.flags.replace(/[gy]/g, ''));
  return descriptor<string>('string', {
    constraints: [
      {
        name: 'pattern',
        value: regex.source,
        validate: (v) => typeof v === 'string' && safe.test(v),
        message: `must match pattern ${regex}`,
      },
    ],
  });
}

// ---------------------------------------------------------------------------
// OneOf — constrained string literals
// ---------------------------------------------------------------------------

/** Restrict a string to one of the given literal values */
export function OneOf<const V extends string[]>(...values: V): SchemaDescriptor<V[number]> {
  return descriptor<V[number]>('string', {
    constraints: [
      {
        name: 'oneOf',
        value: values,
        validate: (v) => typeof v === 'string' && values.includes(v as V[number]),
        message: `must be one of: ${values.join(', ')}`,
      },
    ],
  });
}

// ---------------------------------------------------------------------------
// Constrained — compose multiple constraint descriptors
// ---------------------------------------------------------------------------

/**
 * Merge multiple constraint descriptors into a single descriptor.
 *
 * Each argument must be a `SchemaDescriptor` (e.g. `MinLength(8)`, `MaxLength(32)`, `Email`).
 * Constraints from all descriptors are combined; the first descriptor provides the base type.
 *
 * @example
 * ```typescript
 * const password = Constrained(MinLength(8), MaxLength(32));
 * const shortEmail = Constrained(Email, MaxLength(100));
 * ```
 */
export function Constrained<T>(...descriptors: SchemaDescriptor<T>[]): SchemaDescriptor<T> {
  if (descriptors.length === 0) {
    throw new Error('Constrained() requires at least one descriptor');
  }
  const base = descriptors[0];
  const allConstraints = descriptors.flatMap((d) => d.constraints ?? []);
  return { ...base, constraints: allConstraints };
}

// ---------------------------------------------------------------------------
// Default — static fallback value
// ---------------------------------------------------------------------------

/** Provide a default value. The field is optional in input but always present in output. */
export function Default<P extends SchemaPrimitive, D extends InferPrimitive<P>>(
  type: P,
  value: D,
): SchemaDescriptor<InferPrimitive<P>> {
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, default: value } as SchemaDescriptor<InferPrimitive<P>>;
}

// ---------------------------------------------------------------------------
// Env — resolve value from environment variable
// ---------------------------------------------------------------------------

/** Read the value from an environment variable. Falls through to YAML / default if unset. */
export function Env<P extends SchemaPrimitive>(envVar: string, type: P): SchemaDescriptor<InferPrimitive<P>> {
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, env: envVar } as SchemaDescriptor<InferPrimitive<P>>;
}

// ---------------------------------------------------------------------------
// Resolve — async value resolution at bootstrap
// ---------------------------------------------------------------------------

/** Resolve the value asynchronously at bootstrap (e.g. from a secret manager). */
export function Resolve<P extends SchemaPrimitive>(
  fn: () => Promise<unknown>,
  type: P,
): SchemaDescriptor<InferPrimitive<P>> {
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, resolve: fn } as SchemaDescriptor<InferPrimitive<P>>;
}

// ---------------------------------------------------------------------------
// Sensitive — redaction marker
// ---------------------------------------------------------------------------

/** Mark a field as sensitive. Validation errors will redact the value. */
export function Sensitive<P extends SchemaPrimitive>(type: P): SchemaDescriptor<InferPrimitive<P>> {
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, sensitive: true } as SchemaDescriptor<InferPrimitive<P>>;
}

/**
 * Mark a field whose fallback default is unsafe in production — an in-memory
 * store, a process-generated key, a permissive transport. The marker is carried
 * into the extracted config schema (`productionUnsafeDefault`) so `putnami
 * doctor` can flag the field when it is left unset in the production config
 * sources. Twin of the Go `productionUnsafeDefault:"true"` struct tag.
 */
export function ProductionUnsafeDefault<P extends SchemaPrimitive>(type: P): SchemaDescriptor<InferPrimitive<P>> {
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, productionUnsafeDefault: true } as SchemaDescriptor<InferPrimitive<P>>;
}

// ---------------------------------------------------------------------------
// Desc — documentation description
// ---------------------------------------------------------------------------

/**
 * Add a human-readable description to a schema field.
 * The description is used in OpenAPI documentation.
 *
 * @example
 * ```typescript
 * const UserSchema = {
 *   name: Desc('Full name of the user', String),
 *   email: Desc('Primary email address', Email),
 *   age: Desc('Age in years', Optional(Int)),
 * };
 * ```
 */
export function Desc<P extends SchemaPrimitive>(description: string, type: P): SchemaDescriptor<InferPrimitive<P>> {
  if (isNestedSchema(type)) {
    // Wrap the nested object in a descriptor that carries the description.
    // The validator and config extractor recognize `schema` on an object
    // descriptor and recurse into it, so the nested shape is preserved.
    return descriptor<InferPrimitive<P>>('object', {
      description,
      schema: type as NestedSchema,
    });
  }
  const base = isSchemaDescriptor(type) ? { ...type } : descriptor<InferPrimitive<P>>(baseTypeName(type));
  return { ...base, description } as SchemaDescriptor<InferPrimitive<P>>;
}
