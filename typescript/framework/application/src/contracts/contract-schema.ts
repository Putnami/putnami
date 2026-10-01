import {
  ArrayOf,
  Optional,
  type SchemaDefinition,
  type SchemaConstraint,
  type SchemaDescriptor,
  type SchemaPrimitive,
  validateSchema,
} from '@putnami/runtime';
import type { ContractField, ContractManifest, ContractStruct, ContractUnion } from './contract-ir.types';

const CONTRACT_TYPE = Symbol.for('putnami:contract-type');

export interface ContractTypeReference {
  readonly manifest: ContractManifest;
  readonly name: string;
}

type Marked = { readonly [CONTRACT_TYPE]: ContractTypeReference };

/**
 * Build a runtime schema property directly from a canonical contract node.
 *
 * Use contractStruct() when a DTO is the complete endpoint `.body()` or
 * `.returns()` schema. A non-string marker carries the canonical node identity
 * through Optional/ArrayOf wrappers without appearing in user payloads, allowing
 * the OpenAPI generator to emit a `$ref` instead of re-declaring the shape.
 */
export function contractType(manifest: ContractManifest, name: string): SchemaDefinition | SchemaDescriptor {
  const node = findNode(manifest, name);
  if (!node) {
    throw new Error(`contract type ${JSON.stringify(name)} is not declared by ${JSON.stringify(manifest.name)}`);
  }

  const structNode = manifest.structs?.find((candidate) => candidate.name === name);
  if (structNode) {
    return structDefinition(manifest, structNode);
  }

  return referenceDescriptor(manifest, name);
}

/** Build an endpoint-compatible object schema from a canonical contract DTO. */
export function contractStruct(manifest: ContractManifest, name: string): SchemaDefinition {
  const structNode = manifest.structs?.find((candidate) => candidate.name === name);
  if (!structNode) {
    if (findNode(manifest, name)) {
      throw new Error(`contract type ${JSON.stringify(name)} is not a struct`);
    }
    throw new Error(`contract type ${JSON.stringify(name)} is not declared by ${JSON.stringify(manifest.name)}`);
  }
  return structDefinition(manifest, structNode);
}

/** Return the canonical reference carried by a contract-derived schema value. */
export function getContractTypeReference(value: unknown): ContractTypeReference | undefined {
  if ((typeof value !== 'object' && typeof value !== 'function') || value === null) return undefined;
  return (value as Partial<Marked>)[CONTRACT_TYPE];
}

function findNode(manifest: ContractManifest, name: string) {
  return (
    manifest.enums?.find((node) => node.name === name) ??
    manifest.unions?.find((node) => node.name === name) ??
    manifest.structs?.find((node) => node.name === name)
  );
}

function referenceDescriptor(manifest: ContractManifest, name: string): SchemaDescriptor & Marked {
  const enumNode = manifest.enums?.find((node) => node.name === name);
  const unionNode = manifest.unions?.find((node) => node.name === name);
  const baseType = enumNode ? 'string' : 'object';
  let constraints: SchemaConstraint[] | undefined;
  if (enumNode) {
    constraints = [
      {
        name: 'oneOf',
        value: enumNode.values.map((value) => value.value),
        validate: (value: unknown) =>
          typeof value === 'string' && enumNode.values.some((candidate) => candidate.value === value),
        message: `must be one of: ${enumNode.values.map((value) => value.value).join(', ')}`,
      },
    ];
  } else if (unionNode) {
    constraints = [unionConstraint(manifest, unionNode)];
  }
  return {
    __schema: 'putnami:schema',
    baseType,
    ...(constraints ? { constraints } : {}),
    [CONTRACT_TYPE]: { manifest, name },
  } as SchemaDescriptor & Marked;
}

function structDefinition(manifest: ContractManifest, structNode: ContractStruct): SchemaDefinition & Marked {
  const definition = {
    [CONTRACT_TYPE]: { manifest, name: structNode.name },
  } as SchemaDefinition & Marked;
  for (const field of structNode.fields ?? []) {
    definition[field.name] = fieldPrimitive(manifest, field);
  }
  return definition;
}

function unionConstraint(manifest: ContractManifest, unionNode: ContractUnion): SchemaConstraint {
  const tags = unionNode.variants.map((variant) => variant.tag);
  return {
    name: 'contractUnion',
    value: { discriminator: unionNode.discriminator, tags },
    validate: (value: unknown) => {
      if (!isObjectRecord(value)) return false;
      const variant = unionNode.variants.find((candidate) => candidate.tag === value[unionNode.discriminator]);
      if (!variant) return false;
      const fields = variant.fields ?? manifest.structs?.find((candidate) => candidate.name === variant.struct)?.fields;
      if (!fields) return false;
      const schema: SchemaDefinition = {};
      for (const field of fields) {
        if (field.name !== unionNode.discriminator) schema[field.name] = fieldPrimitive(manifest, field);
      }
      return validateSchema(schema, value, { label: unionNode.name }).errors.length === 0;
    },
    message: `must be a valid ${unionNode.name} tagged by ${unionNode.discriminator}: ${tags.join(', ')}`,
  };
}

function isObjectRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function fieldPrimitive(manifest: ContractManifest, field: ContractField): SchemaPrimitive {
  let primitive: SchemaPrimitive;
  switch (field.type) {
    case 'string':
    case 'duration':
      primitive = String;
      break;
    case 'int':
    case 'float':
      primitive = Number;
      break;
    case 'bool':
      primitive = Boolean;
      break;
    default:
      primitive = referenceDescriptor(manifest, field.type);
  }
  if (field.repeated) primitive = ArrayOf(primitive);
  if (field.optional) primitive = Optional(primitive);
  return primitive;
}
