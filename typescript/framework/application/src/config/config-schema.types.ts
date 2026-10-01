/**
 * Shared types and the canonical field-type vocabulary for config schema
 * extraction, canonicalization, and JSON Schema emission.
 *
 * Kept in its own module so the extract / canonicalize / json-schema modules
 * can share them without importing each other (avoiding cycles).
 */

/**
 * Canonical field type vocabulary shared with Go extractors.
 * Both extractors must map language-specific types to one of these values.
 */
export const FIELD_TYPES = {
  string: 'string',
  int: 'int',
  float: 'float',
  bool: 'bool',
  duration: 'duration',
  object: 'object',
  array: 'array',
  map: 'map',
} as const;

export const FIELD_TYPE_VALUES = [
  FIELD_TYPES.string,
  FIELD_TYPES.int,
  FIELD_TYPES.float,
  FIELD_TYPES.bool,
  FIELD_TYPES.duration,
  FIELD_TYPES.object,
  FIELD_TYPES.array,
  FIELD_TYPES.map,
] as const;

export const MAP_KEY_TYPE_VALUES = [FIELD_TYPES.string, FIELD_TYPES.int, FIELD_TYPES.bool] as const;

export const VALID_MAP_KEY_TYPES = new Set<string>(MAP_KEY_TYPE_VALUES);

export interface ConfigSchemaManifest {
  appName: string;
  version: string;
  schemaHash: string;
  configs: ConfigBlock[];
}

export interface ConfigBlock {
  path: string;
  fields: FieldSchema[];
  /** Opt-out for blocks whose presence at the top of the resolved tree is contingent. */
  optional?: boolean;
}

export interface FieldSchema {
  name: string;
  type: string;
  description?: string;
  required?: boolean;
  default?: string;
  env?: string;
  sensitive?: boolean;
  /**
   * Marks a field whose fallback default is unsafe in production (an in-memory
   * store, a process-generated key, a permissive transport). Doctor flags such a
   * field when it is left unset in the production config sources. Additive and
   * serialized only when true, immediately after `sensitive`, so the canonical
   * hash of an existing field is unchanged and a set marker hashes identically to
   * the Go twin (`protocols/config/config.go`).
   */
  productionUnsafeDefault?: boolean;
  constraints?: string[];
  // Composite slots — only one is populated, depending on type.
  fields?: FieldSchema[]; // type=object
  items?: FieldSchema; // type=array
  keys?: string; // type=map (primitive key type)
  values?: FieldSchema; // type=map
}

export interface JsonSchemaManifest {
  appName: string;
  configs: ConfigBlock[];
}
