import { type FieldSchema, FIELD_TYPES, type JsonSchemaManifest } from './config-schema.types';

/** Dialect emitted by `renderJSONSchema`. Mirrors the Go protocol package. */
export const JSON_SCHEMA_DRAFT = 'https://json-schema.org/draft/2020-12/schema';

/**
 * Convert a config schema manifest into a JSON Schema (Draft 2020-12)
 * describing the resolved config tree. Both Go and TS implementations
 * must produce semantically equivalent output — the shared fixtures
 * `protocols/config/fixtures/valid/*.jsonschema.json` lock this
 * invariant.
 */
export function renderJSONSchema(manifest: JsonSchemaManifest): Record<string, unknown> {
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  for (const block of manifest.configs) {
    properties[block.path] = renderObjectFields(block.fields);
    // Blocks describe the contract every config file is expected to populate;
    // the JSON Schema marks them required unless the schema author explicitly
    // opted out via Block.Optional. Mirrors the Go renderer.
    if (!block.optional) {
      required.push(block.path);
    }
  }
  required.sort();
  const out: Record<string, unknown> = {
    $schema: JSON_SCHEMA_DRAFT,
    type: 'object',
    properties,
    additionalProperties: false,
  };
  if (required.length > 0) {
    out['required'] = required;
  }
  if (manifest.appName) {
    out['title'] = `${manifest.appName} config`;
  }
  return out;
}

function renderObjectFields(fields: FieldSchema[]): Record<string, unknown> {
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  for (const f of fields) {
    properties[f.name] = renderJsonSchemaField(f);
    if (f.required) {
      required.push(f.name);
    }
  }
  required.sort();
  const out: Record<string, unknown> = {
    type: 'object',
    properties,
    additionalProperties: false,
  };
  if (required.length > 0) {
    out['required'] = required;
  }
  return out;
}

function renderJsonSchemaField(f: FieldSchema): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  switch (f.type) {
    case FIELD_TYPES.string:
    case FIELD_TYPES.duration:
      out['type'] = 'string';
      if (f.type === FIELD_TYPES.duration) {
        out['format'] = 'duration';
      }
      break;
    case FIELD_TYPES.int:
      out['type'] = 'integer';
      break;
    case FIELD_TYPES.float:
      out['type'] = 'number';
      break;
    case FIELD_TYPES.bool:
      out['type'] = 'boolean';
      break;
    case FIELD_TYPES.object: {
      const nested = renderObjectFields(f.fields ?? []);
      for (const [k, v] of Object.entries(nested)) {
        out[k] = v;
      }
      break;
    }
    case FIELD_TYPES.array:
      out['type'] = 'array';
      if (f.items) {
        out['items'] = renderJsonSchemaField(f.items);
      }
      break;
    case FIELD_TYPES.map:
      out['type'] = 'object';
      if (f.values) {
        out['additionalProperties'] = renderJsonSchemaField(f.values);
      }
      if (f.keys) {
        out['propertyNames'] = mapKeyConstraint(f.keys);
      }
      break;
    default:
      out['type'] = 'object';
  }

  if (f.description) out['description'] = f.description;
  // `default` is a transport string at every layer (serialized with omitempty
  // on the Go side). An empty string is therefore indistinguishable from "no
  // default declared", so both runtimes skip it — including an empty-string
  // default on a string field. `if (f.default)` is false for both `undefined`
  // and `''`, mirroring Go's `if f.Default != ""`. When a default is present,
  // coerceDefault renders it as the JSON scalar matching the declared type so
  // the emitted `default` validates against its own typed schema. This rule and
  // the coercion must stay identical to renderField in the Go twin
  // (protocols/config/jsonschema.go).
  if (f.default) out['default'] = coerceDefault(f.type, f.default);
  if (f.sensitive) out['x-sensitive'] = true;
  if (f.productionUnsafeDefault) out['x-production-unsafe-default'] = true;
  if (f.env) out['x-env'] = f.env;
  if (f.constraints && f.constraints.length > 0) {
    out['x-constraints'] = [...f.constraints];
  }
  return out;
}

/**
 * Gates which transport strings are coerced to a JSON number for float fields.
 * Admits exactly the JSON-number shape (optional sign, integer/fraction,
 * optional exponent) and rejects Go/JS-specific spellings that would parse
 * differently across languages (hex floats, `Inf`/`NaN`, surrounding
 * whitespace). The identical pattern is applied on the Go side
 * (protocols/config/jsonschema.go) so both runtimes accept and reject the same
 * tokens.
 */
const FLOAT_DEFAULT_PATTERN = /^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$/;

/**
 * Parse a transport-string default into the JSON scalar that matches the
 * field's declared type, so a generated default validates against its own typed
 * schema (e.g. an int field emits the number 604800, not the string "604800").
 * Duration stays a string (it already carries `format: "duration"`);
 * string/object/array/map pass through unchanged.
 *
 * Parsing is intentionally narrow and byte-for-byte identical to the Go twin
 * `coerceDefault` in protocols/config/jsonschema.go: bool accepts only
 * "true"/"false"; int accepts an optionally-signed run of digits within the JS
 * safe-integer range; float accepts a JSON-number-shaped, finite token. On any
 * parse failure the raw string is returned unchanged — malformed defaults are
 * surfaced by validation, not repaired here.
 */
function coerceDefault(fieldType: string, raw: string): unknown {
  switch (fieldType) {
    case FIELD_TYPES.bool:
      if (raw === 'true') return true;
      if (raw === 'false') return false;
      return raw;
    case FIELD_TYPES.int:
      if (/^[+-]?\d+$/.test(raw)) {
        const n = Number(raw);
        if (Number.isSafeInteger(n)) return n;
      }
      return raw;
    case FIELD_TYPES.float:
      if (FLOAT_DEFAULT_PATTERN.test(raw)) {
        const n = Number(raw);
        if (Number.isFinite(n)) return n;
      }
      return raw;
    default:
      return raw;
  }
}

function mapKeyConstraint(keyType: string): Record<string, unknown> {
  switch (keyType) {
    case FIELD_TYPES.int:
      return { type: 'string', pattern: '^-?[0-9]+$' };
    case FIELD_TYPES.bool:
      return { type: 'string', enum: ['true', 'false'] };
    default:
      return { type: 'string' };
  }
}
