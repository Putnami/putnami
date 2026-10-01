import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { ConfigDefinition, SchemaDefinition, SchemaDescriptor, SchemaPrimitive } from '@putnami/runtime';
import { getRegisteredConfigDefinitions, isNestedSchema, isSchemaDescriptor } from '@putnami/runtime';
import { getCurrentProject, joinPath } from '@putnami/utils';
import { computeSchemaHash } from './config-schema-canonicalize';
import { renderJSONSchema } from './config-schema-json';
import {
  type ConfigBlock,
  type ConfigSchemaManifest,
  type FieldSchema,
  FIELD_TYPES,
  VALID_MAP_KEY_TYPES,
} from './config-schema.types';

export type { ConfigDefinition };

// Re-export the canonicalization, JSON Schema, and shared types so existing
// importers of this module keep working after the split.
export { computeSchemaHash } from './config-schema-canonicalize';
export { JSON_SCHEMA_DRAFT, renderJSONSchema } from './config-schema-json';
export type { ConfigBlock, ConfigSchemaManifest, FieldSchema } from './config-schema.types';

/**
 * Security-relevant middleware config keys that are configured programmatically
 * (via middleware options rather than `configToken()`), surfaced here in the
 * same {@link FieldSchema} shape the extractor emits so the config doctor can
 * inspect them uniformly with schema-derived fields.
 *
 * This is an **additive** registry: append entries, never refactor the
 * extractor around it. Middleware options are declared where the middleware
 * lives, so a security field that is not schema-derived would otherwise be
 * invisible to the config doctor. The first entry is the rate-limit
 * trusted-proxy allowlist.
 */
export const MIDDLEWARE_SECURITY_CONFIG_FIELDS: readonly FieldSchema[] = [
  {
    name: 'trustedProxies',
    type: FIELD_TYPES.array,
    items: { name: '', type: FIELD_TYPES.string },
    description:
      'Trusted reverse-proxy IPs / CIDR ranges whose X-Forwarded-For is honored for rate-limit keying. ' +
      'Empty ⇒ the socket peer is used and X-Forwarded-For is never trusted.',
  },
];

/**
 * Project-relative manifest locations. Exported so the config-extract hook
 * can probe the same paths the extractor writes (and the Go-side runner
 * peeks — see `typescript/extension/cmd/putnami-ts/config_extract.go`).
 */
export const DEFAULT_OUTPUT_PATH = 'schema/config.json';
export const FALLBACK_OUTPUT_PATH = '.gen/config-schema.json';

/** Options for `extractConfigSchema`. */
interface ExtractConfigSchemaOptions {
  /** Application name written into the manifest. Defaults to `project.name`. */
  appName?: string;
  /** Application version written into the manifest. Defaults to `APP_VERSION` env. */
  version?: string;
  /**
   * Output path relative to the project root.
   *
   * - `string` (default `'schema/config.json'`): emit to a committed path.
   * - `false`: keep the manifest in `.gen/config-schema.json` (gitignored).
   */
  output?: string | false;
  /**
   * Explicit source of ConfigDefinitions to extract.
   *
   * Falls back to `getRegisteredConfigDefinitions()` (the global registry
   * populated by `configToken()`) when omitted. Use this in tests or
   * tooling that needs to extract a specific schema set without
   * touching the process-wide registry.
   */
  definitions?: ConfigDefinition[];
}

/**
 * Extract config schema from all registered ConfigDefinitions and write to
 * `<project>/schema/config.json` (committed) by default. Pass `output: false` to keep the
 * manifest in `.gen/config-schema.json` instead.
 *
 * Call this after all Config() definitions have been imported (e.g. during the generate or
 * warmup phase).
 */
export function extractConfigSchema(options: ExtractConfigSchemaOptions = {}): ConfigSchemaManifest | undefined {
  const definitions = options.definitions ?? getRegisteredConfigDefinitions();
  if (definitions.length === 0) {
    return undefined;
  }

  const configs: ConfigBlock[] = [];
  const errors: string[] = [];
  for (const def of definitions) {
    const block = extractBlock(def, errors);
    if (block) {
      configs.push(block);
    }
  }

  if (errors.length > 0) {
    throw new Error(`config schema extraction failed:\n  - ${errors.join('\n  - ')}`);
  }

  if (configs.length === 0) {
    return undefined;
  }

  const project = getCurrentProject();
  const resolvedAppName = options.appName || project?.name || '';
  const resolvedVersion = options.version || process.env['APP_VERSION'] || '';

  const schemaHash = computeSchemaHash(configs);

  const manifest: ConfigSchemaManifest = {
    appName: resolvedAppName,
    version: resolvedVersion,
    schemaHash,
    configs,
  };

  if (project) {
    const relative = options.output === false ? FALLBACK_OUTPUT_PATH : (options.output ?? DEFAULT_OUTPUT_PATH);
    const outPath = joinPath(project.path, relative);
    mkdirSync(dirname(outPath), { recursive: true });
    writeFileSync(outPath, JSON.stringify(manifest, null, 2));

    // Emit the companion JSON Schema document next to the manifest so
    // editor validation (VS Code YAML extension, ajv-based publish-time
    // checks, …) can consume the schema without putnami-specific
    // knowledge. The Go extractor writes the same companion path.
    const jsonSchemaPath = jsonSchemaCompanionPath(outPath);
    writeFileSync(jsonSchemaPath, JSON.stringify(renderJSONSchema(manifest), null, 2));
  }

  return manifest;
}

function jsonSchemaCompanionPath(manifestPath: string): string {
  const dot = manifestPath.lastIndexOf('.');
  if (dot === -1) return `${manifestPath}.jsonschema`;
  return `${manifestPath.slice(0, dot)}.jsonschema${manifestPath.slice(dot)}`;
}

function extractBlock(def: ConfigDefinition, errors: string[]): ConfigBlock | undefined {
  const fields = extractSchemaFields(def.schema, new Set(), def.path, errors);
  return { path: def.path, fields };
}

/**
 * Extract a single {@link ConfigBlock} (path + fields, with sensitive flags)
 * from one {@link ConfigDefinition}, without writing any manifest to disk.
 *
 * This is the side-effect-free seam the capabilities producer reuses so its
 * config fields — and their sensitive flags — match the published config schema
 * exactly. Throws when the definition cannot be extracted (e.g. a non-primitive
 * map key), mirroring {@link extractConfigSchema}'s aggregate error.
 */
export function extractConfigBlock(def: ConfigDefinition): ConfigBlock {
  const errors: string[] = [];
  const block = extractBlock(def, errors);
  if (errors.length > 0) {
    throw new Error(`config schema extraction failed:\n  - ${errors.join('\n  - ')}`);
  }
  return block ?? { path: def.path, fields: [] };
}

/**
 * Walk a SchemaDefinition and produce FieldSchemas, recursing into nested
 * schemas, array items, and map values. The `seen` set guards against
 * recursive schemas: when the same nested schema reference appears further
 * down the stack we emit an opaque object so resolution terminates with
 * the same shape an unresolvable type would produce.
 */
function extractSchemaFields(
  schema: SchemaDefinition,
  seen: Set<object>,
  path: string,
  errors: string[],
): FieldSchema[] {
  const fields: FieldSchema[] = [];
  for (const [key, descriptor] of Object.entries(schema)) {
    const field = buildField(key, descriptor, seen, `${path}.${key}`, errors);
    fields.push(field);
  }
  return fields;
}

function buildField(
  name: string,
  descriptor: SchemaPrimitive,
  seen: Set<object>,
  path: string,
  errors: string[],
): FieldSchema {
  const field: FieldSchema = { name, type: '' };

  if (isSchemaDescriptor(descriptor)) {
    applyDescriptorShape(field, descriptor, seen, path, errors);
    applyDescriptorMeta(field, descriptor);
  } else if (isNestedSchema(descriptor)) {
    field.type = FIELD_TYPES.object;
    field.fields = enterNested(descriptor, seen, path, errors);
  } else {
    field.type = resolvePrimitiveType(descriptor);
  }

  return field;
}

/**
 * Populate the type + composite slots from a SchemaDescriptor. Arrays and
 * maps are described inside the descriptor (array/map flags + items/value
 * primitives); scalar descriptors carry a baseType we map to the canonical
 * vocabulary.
 */
function applyDescriptorShape(
  field: FieldSchema,
  descriptor: SchemaDescriptor,
  seen: Set<object>,
  path: string,
  errors: string[],
): void {
  if (descriptor.array && descriptor.items !== undefined) {
    field.type = FIELD_TYPES.array;
    field.items = resolvePrimitive(descriptor.items, seen, path, errors);
    return;
  }
  if (descriptor.schema) {
    // Desc(text, { ... }) wraps a nested object in a descriptor that
    // carries description metadata. Treat it as an object field and
    // recurse into the schema. Matches the Go side, where `desc:"..."`
    // on a nested struct field works the same way.
    field.type = FIELD_TYPES.object;
    field.fields = enterNested(descriptor.schema, seen, path, errors);
    return;
  }
  if (descriptor.map) {
    field.type = FIELD_TYPES.map;
    const keyType = descriptor.mapKey === undefined ? '' : resolvePrimitiveType(descriptor.mapKey);
    if (keyType === '' || !VALID_MAP_KEY_TYPES.has(keyType)) {
      errors.push(
        `${path}: map key type ${JSON.stringify(keyType)} is not a primitive; map keys must be string, int, or bool`,
      );
      field.keys = FIELD_TYPES.string;
    } else {
      field.keys = keyType;
    }
    if (descriptor.mapValue !== undefined) {
      field.values = resolvePrimitive(descriptor.mapValue, seen, path, errors);
    } else {
      field.values = { name: '', type: FIELD_TYPES.object };
    }
    return;
  }

  field.type = mapBaseType(descriptor.baseType);
}

function applyDescriptorMeta(field: FieldSchema, descriptor: SchemaDescriptor): void {
  if (descriptor.env) {
    field.env = descriptor.env;
  }
  if (descriptor.default !== undefined) {
    field.default = String(descriptor.default);
  }
  if (descriptor.sensitive) {
    field.sensitive = true;
  }
  if (descriptor.productionUnsafeDefault) {
    field.productionUnsafeDefault = true;
  }
  if (descriptor.description) {
    field.description = descriptor.description;
  }
  // `required` is opt-in across both languages: it is only serialized when
  // there is an explicit positive signal (e.g. a `validate:"required"` tag
  // on the Go side, or a Constrained() descriptor with a required
  // constraint on the TS side). Optional() leaves required absent so the
  // canonical JSON form is byte-identical to a Go field with no required
  // signal — the JSON Schema emitter treats "no required field" as "not
  // required" and the cross-language hash stays deterministic.
  if (descriptor.constraints && descriptor.constraints.length > 0) {
    field.constraints = descriptor.constraints.map((c) => c.name);
  }
}

/**
 * Resolve a SchemaPrimitive (descriptor / nested schema / JS builtin) into a
 * FieldSchema with an empty name — used for unnamed children (array items,
 * map values).
 */
function resolvePrimitive(primitive: SchemaPrimitive, seen: Set<object>, path: string, errors: string[]): FieldSchema {
  const child: FieldSchema = { name: '', type: '' };
  if (isSchemaDescriptor(primitive)) {
    applyDescriptorShape(child, primitive, seen, path, errors);
    // Children carry shape only — metadata (env/default/required/constraints)
    // describes a named field, not an unnamed slot.
    if (primitive.constraints && primitive.constraints.length > 0) {
      child.constraints = primitive.constraints.map((c) => c.name);
    }
  } else if (isNestedSchema(primitive)) {
    child.type = FIELD_TYPES.object;
    child.fields = enterNested(primitive, seen, path, errors);
  } else {
    child.type = resolvePrimitiveType(primitive);
  }
  return child;
}

function enterNested(nested: SchemaDefinition, seen: Set<object>, path: string, errors: string[]): FieldSchema[] {
  if (seen.has(nested)) {
    return [];
  }
  seen.add(nested);
  try {
    return extractSchemaFields(nested, seen, path, errors);
  } finally {
    seen.delete(nested);
  }
}

function resolvePrimitiveType(primitive: SchemaPrimitive): string {
  if (primitive === String) return FIELD_TYPES.string;
  if (primitive === Number) return FIELD_TYPES.int;
  if (primitive === Boolean) return FIELD_TYPES.bool;
  if (isSchemaDescriptor(primitive)) {
    return mapBaseType(primitive.baseType);
  }
  if (isNestedSchema(primitive)) {
    return FIELD_TYPES.object;
  }
  return FIELD_TYPES.string;
}

function mapBaseType(baseType: string): string {
  if (baseType === 'number' || baseType === 'integer') return FIELD_TYPES.int;
  if (baseType === 'float' || baseType === 'double') return FIELD_TYPES.float;
  if (baseType === 'boolean') return FIELD_TYPES.bool;
  if (baseType in FIELD_TYPES) return baseType;
  // Unknown baseType maps to object — same behavior as a struct the
  // resolver could not expand on the Go side.
  return FIELD_TYPES.object;
}
