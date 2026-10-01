import { isSchemaDescriptor, type SchemaConstraint, type SchemaDefinition, type SchemaPrimitive } from './schema';

/**
 * Build-time AOT validator compiler (shared across plugins: API, web, streams).
 *
 * Emits a specialised, readable validator function — `(raw, fallback) => validated`
 * — for a schema, inlining the happy path for the field shapes it can replicate
 * exactly: the scalar types `String`, `Number`, `Boolean`, `Int`, plus
 * `Optional(...)`, `Default(...)`, and `ArrayOf(...)` of those scalars. On ANY
 * anomaly (missing required field, wrong type, non-integer, …) the emitted code
 * delegates to `fallback` — the generic `validateSchema` — so error messages and
 * edge cases stay byte-identical to the interpreted path. Validation errors are
 * the cold path; only the valid-input fast path is inlined.
 *
 * Regex-constrained types (`Uuid`, `Email`, `Url`, `DateIso`, `Pattern`) are
 * deliberately NOT inlined: a hardcoded pattern could drift from the schema's and
 * silently accept invalid input. They (and anything else) make the whole schema
 * `inlineable: false`, so the caller keeps the generic validator.
 */

export interface CompiledValidator {
  /** True only when every field could be inlined. */
  readonly inlineable: boolean;
  /** Source of an arrow function `(raw, fallback) => Record<string, unknown>`. Empty when not inlineable. */
  readonly source: string;
}

export interface CompileOptions {
  /** Coerce string inputs (params / query) to number/boolean, mirroring `validateSchema`. */
  readonly coerce: boolean;
}

/**
 * Runtime shape of a build-time-compiled (AOT) field validator. Materialised as
 * normal bundled code (never `new Function` at runtime); validates the fast path
 * inline and delegates to `fallback` (the generic `validateSchema`) on any anomaly.
 */
export type AotFieldValidator = (
  raw: unknown,
  fallback: (raw: unknown) => Record<string, unknown>,
) => Record<string, unknown>;

/** Per-input compiled validators for a route/endpoint (any subset). */
export interface AotValidators {
  readonly params?: AotFieldValidator;
  readonly query?: AotFieldValidator;
  readonly body?: AotFieldValidator;
}

type ScalarKind = 'string' | 'number' | 'boolean' | 'integer';

type FieldPlan =
  | {
      readonly form: 'scalar';
      readonly scalar: ScalarKind;
      readonly optional: boolean;
      readonly defaultValue?: unknown;
      readonly hasDefault: boolean;
    }
  | { readonly form: 'array'; readonly item: ScalarKind };

/** Classify a scalar base type + constraints, or `undefined` if not safely replicable. */
function classifyScalar(baseType: string, constraints?: readonly SchemaConstraint[]): ScalarKind | undefined {
  if (baseType !== 'string' && baseType !== 'number' && baseType !== 'boolean') {
    return undefined;
  }
  if (!constraints || constraints.length === 0) {
    return baseType as ScalarKind;
  }
  // The canonical `Int` integer check is the only constraint we replicate exactly.
  if (baseType === 'number' && constraints.length === 1 && constraints[0]?.name === 'integer') {
    return 'integer';
  }
  return undefined;
}

/** Classify a value used as an array item (a builtin or a plain scalar descriptor). */
function classifyItemScalar(item: SchemaPrimitive): ScalarKind | undefined {
  if (item === String) return 'string';
  if (item === Number) return 'number';
  if (item === Boolean) return 'boolean';
  if (!isSchemaDescriptor(item)) return undefined;
  if (item.optional || item.default !== undefined || item.array || item.map || item.schema) return undefined;
  return classifyScalar(item.baseType, item.constraints);
}

/** Classify a schema field into an inline plan, or `undefined` when it must stay generic. */
function classifyField(prop: SchemaPrimitive): FieldPlan | undefined {
  if (prop === String) return { form: 'scalar', scalar: 'string', optional: false, hasDefault: false };
  if (prop === Number) return { form: 'scalar', scalar: 'number', optional: false, hasDefault: false };
  if (prop === Boolean) return { form: 'scalar', scalar: 'boolean', optional: false, hasDefault: false };

  if (!isSchemaDescriptor(prop)) {
    return undefined; // nested object schema, etc.
  }
  if (prop.map || prop.schema) {
    return undefined;
  }

  if (prop.array) {
    // An optional/default array adds shape we don't inline yet.
    if (prop.optional || prop.default !== undefined || prop.items === undefined) return undefined;
    const item = classifyItemScalar(prop.items);
    return item ? { form: 'array', item } : undefined;
  }

  const scalar = classifyScalar(prop.baseType, prop.constraints);
  if (!scalar) {
    return undefined;
  }
  return {
    form: 'scalar',
    scalar,
    optional: prop.optional === true,
    defaultValue: prop.default,
    hasDefault: prop.default !== undefined,
  };
}

const pad = (n: number) => ' '.repeat(n);

/** Lines that coerce + validate `v` in place, bailing to `fallback(raw)` on failure. */
function scalarChecks(v: string, kind: ScalarKind, coerce: boolean, indent: number): string[] {
  const p = pad(indent);
  const lines: string[] = [];
  switch (kind) {
    case 'string':
      lines.push(`${p}if (typeof ${v} !== "string") return fallback(raw);`);
      break;
    case 'number':
      if (coerce) lines.push(`${p}if (typeof ${v} === "string") ${v} = Number(${v});`);
      lines.push(`${p}if (typeof ${v} !== "number" || Number.isNaN(${v})) return fallback(raw);`);
      break;
    case 'integer':
      if (coerce) lines.push(`${p}if (typeof ${v} === "string") ${v} = Number(${v});`);
      lines.push(`${p}if (typeof ${v} !== "number" || !Number.isInteger(${v})) return fallback(raw);`);
      break;
    case 'boolean':
      if (coerce) {
        lines.push(`${p}if (${v} === "true") ${v} = true;`);
        lines.push(`${p}else if (${v} === "false") ${v} = false;`);
      }
      lines.push(`${p}if (typeof ${v} !== "boolean") return fallback(raw);`);
      break;
  }
  return lines;
}

/** Emit the validation + assignment for one field into the `out` accumulator. */
function emitField(v: string, key: string, plan: FieldPlan, coerce: boolean): string[] {
  const lines: string[] = [];
  if (plan.form === 'array') {
    lines.push(`    const ${v} = raw[${key}];`);
    lines.push(`    if (!Array.isArray(${v})) return fallback(raw);`);
    lines.push(`    const ${v}o = [];`);
    lines.push(`    for (let ${v}i = 0; ${v}i < ${v}.length; ${v}i++) {`);
    lines.push(`      let ${v}v = ${v}[${v}i];`);
    lines.push(...scalarChecks(`${v}v`, plan.item, coerce, 6));
    lines.push(`      ${v}o.push(${v}v);`);
    lines.push('    }');
    lines.push(`    out[${key}] = ${v}o;`);
    return lines;
  }

  lines.push(`    let ${v} = raw[${key}];`);
  if (plan.hasDefault) {
    lines.push(`    if (${v} === undefined || ${v} === null) ${v} = ${JSON.stringify(plan.defaultValue)};`);
    lines.push(...scalarChecks(v, plan.scalar, coerce, 4));
    lines.push(`    out[${key}] = ${v};`);
  } else if (plan.optional) {
    lines.push(`    if (${v} !== undefined && ${v} !== null) {`);
    lines.push(...scalarChecks(v, plan.scalar, coerce, 6));
    lines.push(`      out[${key}] = ${v};`);
    lines.push('    }');
  } else {
    lines.push(...scalarChecks(v, plan.scalar, coerce, 4));
    lines.push(`    out[${key}] = ${v};`);
  }
  return lines;
}

/**
 * Compile an inline validator for a schema. Returns `inlineable: false` (and empty
 * source) when any field is outside the safely-replicable set.
 */
export function compileSchemaValidator(schema: SchemaDefinition, options: CompileOptions): CompiledValidator {
  const entries = Object.entries(schema);
  if (entries.length === 0) {
    return { inlineable: false, source: '' };
  }

  const body: string[] = [
    '    if (raw === null || typeof raw !== "object") return fallback(raw);',
    '    const out = {};',
  ];
  for (let i = 0; i < entries.length; i++) {
    const [key, prop] = entries[i];
    const plan = classifyField(prop);
    if (plan === undefined) {
      return { inlineable: false, source: '' };
    }
    body.push(...emitField(`v${i}`, JSON.stringify(key), plan, options.coerce));
  }
  body.push('    return out;');

  const source = `(raw, fallback) => {\n${body.join('\n')}\n  }`;
  return { inlineable: true, source };
}
