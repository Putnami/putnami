import {
  baseTypeName,
  isNestedSchema,
  isSchemaDescriptor,
  type SchemaConstraint,
  type SchemaDescriptor,
  type SchemaPrimitive,
} from '@putnami/runtime';
import { DEFAULT_INTEGER_WIDTH, INTEGER_WIDTH_CONSTRAINT } from '../api/route/schema-wire';
import { pascalCase, protoJsonName, toSnakeCase } from './proto-render';

// ---------------------------------------------------------------------------
// Internal types — shared across proto generation modules
// ---------------------------------------------------------------------------

export interface ProtoOptions {
  /** Proto package name (e.g. "myapp.v1") */
  packageName: string;
  /** Optional Go package option for generated Go code */
  goPackage?: string;
}

export interface RpcEntry {
  name: string;
  requestMessage: string;
  responseMessage: string;
  clientStreaming: boolean;
  serverStreaming: boolean;
}

export interface MessageField {
  name: string;
  /**
   * The `json_name` protobuf derives from {@link name}: lower camel case, and
   * the key a Connect JSON payload uses. Derived here, in the emitter that owns
   * the field name, so no consumer re-derives it from a different rule.
   */
  jsonName: string;
  type: string;
  number: number;
  /**
   * Explicit presence (`optional` in proto3): the field is written even when it
   * holds its zero value, and its absence is observable. Implicit-presence
   * fields omit their zero value on the wire and read back as the zero value.
   */
  optional: boolean;
  repeated: boolean;
  comment?: string;
  /** The `oneof` this field belongs to; at most one member is set at a time. */
  oneof?: string;
  /** Map key type (only for map fields) */
  mapKeyType?: string;
  /** Map value type (only for map fields) */
  mapValueType?: string;
}

// ---------------------------------------------------------------------------
// Generation context — accumulates messages and services
// ---------------------------------------------------------------------------

export class GenerationContext {
  readonly options: ProtoOptions;
  readonly messages = new Map<string, MessageField[]>();
  readonly services = new Map<string, RpcEntry[]>();
  readonly enums = new Map<string, string[]>();

  constructor(options: ProtoOptions) {
    this.options = options;
  }

  /** Register a message if it doesn't exist yet. Returns the message name. */
  addMessage(name: string, fields: MessageField[]): string {
    if (!this.messages.has(name)) {
      this.messages.set(name, fields);
    }
    return name;
  }

  /** Register an enum if it doesn't exist yet. Returns the enum name. */
  addEnum(name: string, values: string[]): string {
    if (!this.enums.has(name)) {
      this.enums.set(name, values);
    }
    return name;
  }
}

// ---------------------------------------------------------------------------
// Schema → Proto type mapping
// ---------------------------------------------------------------------------

/**
 * Convert a single SchemaPrimitive to a proto field definition.
 */
export function schemaFieldToProto(
  name: string,
  prop: SchemaPrimitive,
  fieldNumber: number,
  ctx?: GenerationContext,
  messageName?: string,
): MessageField {
  if (isSchemaDescriptor(prop)) {
    return descriptorToProtoField(name, prop, fieldNumber, ctx, messageName);
  }

  if (isNestedSchema(prop)) {
    // Will be replaced with the nested message name by the caller
    return {
      name: toSnakeCase(name),
      jsonName: protoJsonName(toSnakeCase(name)),
      type: 'message',
      number: fieldNumber,
      optional: false,
      repeated: false,
    };
  }

  // JS constructors
  const typeName = baseTypeName(prop);
  return {
    name: toSnakeCase(name),
    jsonName: protoJsonName(toSnakeCase(name)),
    type: protoType(typeName),
    number: fieldNumber,
    optional: false,
    repeated: false,
  };
}

function descriptorToProtoField(
  name: string,
  desc: SchemaDescriptor,
  fieldNumber: number,
  ctx?: GenerationContext,
  messageName?: string,
): MessageField {
  const snake = toSnakeCase(name);

  // Array type
  if (desc.array && desc.items) {
    const itemType = itemProtoType(desc.items);
    return {
      name: snake,
      jsonName: protoJsonName(snake),
      type: itemType,
      number: fieldNumber,
      // A repeated field has no presence of its own: an empty list and an
      // absent list are the same value, so `optional` is never set on one.
      optional: false,
      repeated: true,
    };
  }

  // Map type
  if (desc.map && desc.mapKey && desc.mapValue) {
    const keyType = mapKeyProtoType(desc.mapKey);
    const valueType = mapValueProtoType(desc.mapValue, ctx, messageName, name);
    return {
      name: snake,
      jsonName: protoJsonName(snake),
      type: 'map',
      number: fieldNumber,
      optional: false,
      repeated: false,
      mapKeyType: keyType,
      mapValueType: valueType,
    };
  }

  // Detect constraint-based type overrides
  const constraintType = desc.constraints ? constraintProtoType(desc.constraints, name, ctx, messageName) : undefined;

  const field: MessageField = {
    name: snake,
    jsonName: protoJsonName(snake),
    type: constraintType ?? protoType(desc.baseType),
    number: fieldNumber,
    // A declared-optional field carries explicit presence, so its zero value
    // still reaches the peer and `undefined` stays distinguishable from `0`.
    optional: desc.optional === true,
    repeated: false,
  };

  // Add format comments from constraints (skip if type was overridden — info is in the type itself)
  if (desc.constraints && !constraintType) {
    const hint = constraintComment(desc.constraints);
    if (hint) {
      field.comment = hint;
    }
  }

  return field;
}

/**
 * Determine the proto type for an array item.
 */
function itemProtoType(item: SchemaPrimitive): string {
  if (isSchemaDescriptor(item)) {
    return protoType(item.baseType);
  }
  return protoType(baseTypeName(item));
}

/**
 * Determine the proto type for a map key.
 * Proto3 map keys must be scalar types: string, int32, int64, uint32, uint64, sint32, sint64, fixed32, fixed64, sfixed32, sfixed64, bool.
 */
function mapKeyProtoType(key: SchemaPrimitive): string {
  if (isSchemaDescriptor(key)) {
    if (key.constraints?.some((c) => c.name === 'integer')) return declaredIntegerWidth(key.constraints);
    return protoType(key.baseType);
  }
  return protoType(baseTypeName(key));
}

/**
 * The wire width an integer field declared, or the default.
 *
 * D0.2 / ADR 0004: the width is declared or defaulted, never inferred from the
 * transport carrying it. Choosing 32 bits here because protobuf makes it cheap
 * would truncate identifiers that traverse REST intact.
 */
function declaredIntegerWidth(constraints: readonly SchemaConstraint[]): string {
  const declared = constraints.find((constraint) => constraint.name === INTEGER_WIDTH_CONSTRAINT);
  return typeof declared?.value === 'string' ? declared.value : DEFAULT_INTEGER_WIDTH;
}

/**
 * Determine the proto type for a map value.
 * Map values can be any scalar or message type (but not another map).
 */
function mapValueProtoType(
  value: SchemaPrimitive,
  ctx?: GenerationContext,
  messageName?: string,
  fieldName?: string,
): string {
  if (isSchemaDescriptor(value)) {
    if (value.constraints?.some((c) => c.name === 'integer')) return declaredIntegerWidth(value.constraints);
    return protoType(value.baseType);
  }
  if (isNestedSchema(value) && ctx && messageName && fieldName) {
    // Register nested message for map value
    const nestedName = `${messageName}${pascalCase(fieldName)}Value`;
    registerNestedMessage(ctx, nestedName, value as Record<string, SchemaPrimitive>);
    return nestedName;
  }
  return protoType(baseTypeName(value));
}

/**
 * Map internal type names to proto3 scalar types.
 */
function protoType(type: string): string {
  switch (type) {
    case 'number':
      return 'double';
    case 'boolean':
      return 'bool';
    case 'string':
      return 'string';
    case 'bytes':
    case 'binary':
      return 'bytes';
    default:
      return 'string';
  }
}

/**
 * Override proto type based on schema constraints.
 * Returns the proto type name if a constraint implies a more specific wire type,
 * or undefined to fall back to the default mapping.
 */
function constraintProtoType(
  constraints: readonly SchemaConstraint[],
  fieldName: string,
  ctx?: GenerationContext,
  messageName?: string,
): string | undefined {
  for (const c of constraints) {
    // D0.2 / ADR 0004: an integer's width is the one the field declared, and
    // `int64` when it declared none. Connect therefore carries exactly the range
    // REST carries.
    if (c.name === 'integer') return declaredIntegerWidth(constraints);
    // OneOf → proto enum
    if (c.name === 'oneOf' && ctx && messageName) {
      const values = c.value as string[];
      const enumName = `${messageName}${pascalCase(fieldName)}`;
      ctx.addEnum(enumName, values);
      return enumName;
    }
  }
  return undefined;
}

/**
 * Derive a comment hint from schema constraints.
 */
function constraintComment(constraints: readonly SchemaConstraint[]): string | undefined {
  for (const c of constraints) {
    if (c.name === 'uuid') return 'UUID format';
    if (c.name === 'email') return 'Email format';
    if (c.name === 'url') return 'URL format';
    if (c.name === 'dateIso') return 'ISO 8601 date';
  }
  return undefined;
}

/**
 * Register a nested object schema as a proto message.
 */
export function registerNestedMessage(
  ctx: GenerationContext,
  name: string,
  schema: Record<string, SchemaPrimitive>,
): void {
  const fields: MessageField[] = [];
  let fieldNumber = 1;

  for (const [key, prop] of Object.entries(schema)) {
    const field = schemaFieldToProto(key, prop, fieldNumber++, ctx, name);
    if (isNestedSchema(prop)) {
      const nestedName = `${name}${pascalCase(key)}`;
      registerNestedMessage(ctx, nestedName, prop);
      field.type = nestedName;
    }
    fields.push(field);
  }

  ctx.addMessage(name, fields);
}
