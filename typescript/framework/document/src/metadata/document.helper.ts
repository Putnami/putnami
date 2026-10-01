import {
  type SchemaDefinition,
  type SchemaPrimitive,
  type ValidationError,
  Optional,
  isSchemaDescriptor,
  validateSchema,
} from '@putnami/runtime';
import type {
  CollectionDefinition,
  CollectionField,
  DocumentIdDefinition,
  FieldOptions,
  FieldDefinition,
} from '../collection';
import { isDocumentIdDefinition } from '../collection';
import type { AdapterId } from '../adapter/document.adapter';

function isDateIsoType(type: SchemaPrimitive): boolean {
  if (!isSchemaDescriptor(type)) return false;
  return type.constraints?.some((constraint) => constraint.name === 'dateIso') === true;
}

type FieldEntry = {
  property: string;
  fieldName: string;
  definition: CollectionField;
};

function normalizeIdScalar(value: unknown): string | number | undefined {
  if (typeof value === 'string' || typeof value === 'number') {
    return value;
  }
  return undefined;
}

export class DocumentHelper<T extends CollectionDefinition> {
  private readonly entries: FieldEntry[];
  private readonly idEntries: FieldEntry[];
  private readonly fieldToProperty: Map<string, string>;

  constructor(private collectionDef: T) {
    this.entries = Object.entries(collectionDef.schema).map(([property, definition]) => ({
      property,
      fieldName: definition.options.fieldName ?? property,
      definition,
    }));
    this.idEntries = this.entries.filter((entry) => isDocumentIdDefinition(entry.definition));
    this.fieldToProperty = new Map(this.entries.map((entry) => [entry.fieldName, entry.property]));
  }

  get collectionName(): string {
    return this.collectionDef.collectionName;
  }

  get db(): string | undefined {
    return this.collectionDef.options.db;
  }

  get indexes() {
    return this.collectionDef.options.indexes ?? [];
  }

  get idProperties(): string[] {
    return this.idEntries.map((entry) => entry.property);
  }

  get idFieldNames(): string[] {
    return this.idEntries.map((entry) => entry.fieldName);
  }

  get hasCompositeId(): boolean {
    return this.idEntries.length > 1;
  }

  fieldName(propertyKey: string): string {
    const definition = this.collectionDef.schema[propertyKey];
    return definition?.options.fieldName ?? propertyKey;
  }

  propertyName(fieldName: string): string {
    return this.fieldToProperty.get(fieldName) ?? fieldName;
  }

  hasProperty(propertyKey: string): boolean {
    return propertyKey in this.collectionDef.schema;
  }

  getFieldOptions(propertyKey: string): FieldOptions | undefined {
    return this.collectionDef.schema[propertyKey]?.options;
  }

  get schemaDefinition(): SchemaDefinition {
    const def: SchemaDefinition = {};
    for (const [property, field] of Object.entries(this.collectionDef.schema)) {
      def[property] = field.type;
    }
    return def;
  }

  toDocumentValue(propertyKey: string, value: unknown): unknown {
    const field = this.collectionDef.schema[propertyKey] as FieldDefinition | DocumentIdDefinition | undefined;
    if (!field) return value;
    if (field.options.toDocument) return field.options.toDocument(value);
    if (value instanceof Date && isDateIsoType(field.type)) return value.toISOString();
    return value;
  }

  /**
   * Read-path inverse of {@link toDocumentValue}.
   *
   * Stored `DateIso` values are already ISO strings (that is the entity-facing
   * representation), so the read path passes them through unchanged rather than
   * re-applying the write transform. The one exception is a backend that hydrates
   * a date field as a native `Date` (e.g. a Firestore `Timestamp` converted with
   * `.toDate()`): such values are normalized to the ISO string the entity type
   * expects.
   */
  toEntityValue(propertyKey: string, value: unknown): unknown {
    const field = this.collectionDef.schema[propertyKey] as FieldDefinition | DocumentIdDefinition | undefined;
    if (!field) return value;
    if (field.options.fromDocument) return field.options.fromDocument(value);
    // Normalize native Dates that an adapter may return for a DateIso field to
    // the ISO-string entity representation; ISO strings are returned as-is.
    if (value instanceof Date && isDateIsoType(field.type)) return value.toISOString();
    return value;
  }

  toDocument(document: Record<string, unknown>): Record<string, unknown> {
    const raw: Record<string, unknown> = {};

    for (const entry of this.entries) {
      const value = document[entry.property];
      if (value === undefined) continue;
      raw[entry.fieldName] = this.toDocumentValue(entry.property, value);
    }

    return raw;
  }

  toEntity<E = unknown>(row: Record<string, unknown>): E {
    const entity: Record<string, unknown> = {};

    for (const entry of this.entries) {
      let value = row[entry.fieldName];
      if (value === undefined && entry.fieldName !== entry.property) {
        value = row[entry.property];
      }
      if (value === undefined) continue;
      entity[entry.property] = this.toEntityValue(entry.property, value);
    }

    return entity as E;
  }

  normalizeDocumentId(id: unknown): AdapterId | undefined {
    if (this.idEntries.length === 0) {
      return undefined;
    }

    if (this.idEntries.length === 1) {
      const [entry] = this.idEntries;
      const objectValue =
        typeof id === 'object' && id !== null && !Array.isArray(id)
          ? (id as Record<string, unknown>)[entry.property]
          : id;
      const normalized = normalizeIdScalar(this.toDocumentValue(entry.property, objectValue));
      return normalized;
    }

    if (typeof id !== 'object' || id === null || Array.isArray(id)) {
      return undefined;
    }

    const objectId: Record<string, string | number> = {};
    for (const entry of this.idEntries) {
      const value = normalizeIdScalar(
        this.toDocumentValue(entry.property, (id as Record<string, unknown>)[entry.property]),
      );
      if (value === undefined) return undefined;
      objectId[entry.fieldName] = value;
    }
    return objectId;
  }

  validateDocument(document: Record<string, unknown>): ValidationError[] {
    const partialSchema: SchemaDefinition = {};
    const schemaDef = this.schemaDefinition;
    for (const key of Object.keys(document)) {
      if (key in schemaDef) {
        partialSchema[key] = schemaDef[key];
      }
    }
    return validateSchema(partialSchema, document, { label: this.collectionName }).errors;
  }

  validateFullDocument(document: Record<string, unknown>): ValidationError[] {
    const fullSchema: SchemaDefinition = {};
    for (const [property, field] of Object.entries(this.collectionDef.schema)) {
      let type: SchemaPrimitive = field.type;
      const hasDefault = field.options.default !== undefined;
      const alreadyOptional = isSchemaDescriptor(type) && type.optional === true;
      if (hasDefault && !alreadyOptional) {
        type = Optional(type);
      }
      fullSchema[property] = type;
    }
    const errors = validateSchema(fullSchema, document, { label: this.collectionName }).errors;
    // Reject keys present in the input but absent from the schema. A full
    // (replace) write persists only known fields via `toDocument`, so an
    // unknown/typo'd key would otherwise validate clean and be silently
    // dropped — submitted and persisted documents would differ with no signal.
    for (const key of Object.keys(document)) {
      if (!(key in this.collectionDef.schema)) {
        const field = `${this.collectionName}.${key}`;
        errors.push({ field, message: `${field} is not a known field` });
      }
    }
    return errors;
  }
}

export const documentHelper = <T extends CollectionDefinition>(collectionDef: T): DocumentHelper<T> =>
  new DocumentHelper(collectionDef);
