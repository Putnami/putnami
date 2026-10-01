import {
  type SchemaDefinition,
  type SchemaPrimitive,
  type ValidationError,
  Optional,
  baseTypeName,
  isSchemaDescriptor,
  validateSchema,
} from '@putnami/runtime';
import { RepositoryError, RepositoryErrorCode } from '../errors';
import { type ColumnOptions, isKeyDefinition, type KeyOptions, type TableDefinition, type TableField } from '../table';
import type { DatabaseValue } from '../type.type';
import { assertSafeIdentifier } from './identifier';

/** Check if a schema type has a DateIso constraint (handles Optional(DateIso) too) */
function isDateIsoType(type: SchemaPrimitive): boolean {
  if (!isSchemaDescriptor(type)) return false;
  return type.constraints?.some((c) => c.name === 'dateIso') === true;
}

/**
 * Check whether a schema type resolves to a JS number (`Number`, `Int`, and
 * their `Optional`/constrained wrappers). NUMERIC columns are read back as
 * lossless strings by the driver; columns declared as numbers are coerced to a
 * JS number here so existing numeric round-trips keep working, while columns
 * declared as `String` keep the exact decimal string.
 */
function isNumericType(type: SchemaPrimitive): boolean {
  return baseTypeName(type) === 'number';
}

/**
 * Helper class for entity transformations.
 * Reads metadata from a TableDefinition (no reflect-metadata).
 */
export class EntityHelper<T extends TableDefinition> {
  constructor(private tableDef: T) {
    assertSafeIdentifier(tableDef.tableName, 'tableName');
    for (const [prop, field] of Object.entries(tableDef.schema)) {
      const columnName = (field as TableField).options.columnName ?? prop;
      assertSafeIdentifier(columnName, `column "${prop}"`);
    }
  }

  /** The database table name */
  get tableName(): string {
    return this.tableDef.tableName;
  }

  /** Named datasource (e.g., 'auth', 'analytics'). Undefined for default. */
  get db(): string | undefined {
    return this.tableDef.options.db;
  }

  /**
   * Get the database column name for a property.
   *
   * Throws for unknown properties: their names would otherwise be interpolated
   * verbatim into SQL identifiers (WHERE/INSERT clauses), allowing identifier
   * injection. Known column names are validated as safe identifiers at
   * construction.
   */
  columnName(propertyKey: string): string {
    const field = this.tableDef.schema[propertyKey] as TableField | undefined;
    if (!field) {
      throw new RepositoryError(
        `Unknown property "${propertyKey}" on table "${this.tableName}". Valid properties: ${Object.keys(this.tableDef.schema).join(', ')}`,
        RepositoryErrorCode.Unknown,
      );
    }
    return field.options.columnName ?? propertyKey;
  }

  /** All database column names */
  get columnNames(): string[] {
    return Object.keys(this.tableDef.schema).map((prop) => this.columnName(prop));
  }

  /** Extract primary key values from an entity */
  extractKeys(entity: Record<string, unknown>): Record<string, DatabaseValue> {
    const keys: Record<string, DatabaseValue> = {};

    for (const [prop, field] of Object.entries(this.tableDef.schema)) {
      if (isKeyDefinition(field)) {
        const columnName = (field.options as KeyOptions).columnName ?? prop;
        keys[columnName] = entity[prop] as DatabaseValue;
      }
    }

    return keys;
  }

  /** Get the set of property names that are primary keys */
  get keyProperties(): Set<string> {
    const keys = new Set<string>();
    for (const [prop, field] of Object.entries(this.tableDef.schema)) {
      if (isKeyDefinition(field)) {
        keys.add(prop);
      }
    }
    return keys;
  }

  /** Get the column options for a property (column or key) */
  getColumnOptions(propertyKey: string): ColumnOptions | undefined {
    const field = this.tableDef.schema[propertyKey] as TableField | undefined;
    return field?.options;
  }

  /** Build a SchemaDefinition from the table schema (maps property names to SchemaPrimitives) */
  get schemaDefinition(): SchemaDefinition {
    const def: SchemaDefinition = {};
    for (const [prop, field] of Object.entries(this.tableDef.schema)) {
      def[prop] = field.type;
    }
    return def;
  }

  /** Convert an entity value to a database value, applying toDatabase transform or auto-coercion */
  toDatabaseValue(propertyKey: string, value: unknown): unknown {
    const field = this.tableDef.schema[propertyKey] as TableField | undefined;
    if (!field) return value;
    if (field.options.toDatabase) return field.options.toDatabase(value);
    if (value instanceof Date && isDateIsoType(field.type)) return value.toISOString();
    return value;
  }

  /** Check if a property name exists in the table schema */
  hasProperty(name: string): boolean {
    return name in this.tableDef.schema;
  }

  /** Validate a partial entity against the table schema. Only validates provided fields. */
  validateEntity(entity: Record<string, unknown>): ValidationError[] {
    // Build a schema containing only the fields present in the entity
    const partialSchema: SchemaDefinition = {};
    const schemaDef = this.schemaDefinition;
    for (const key of Object.keys(entity)) {
      if (key in schemaDef) {
        partialSchema[key] = schemaDef[key];
      }
    }
    const { errors } = validateSchema(partialSchema, entity, { label: this.tableName });
    return errors;
  }

  /**
   * Validate an entity against the full table schema, including required field checks.
   * Auto-generated keys and columns with SQL defaults are treated as optional.
   * Use this for insert-style validation where all required fields must be present.
   */
  validateFullEntity(entity: Record<string, unknown>): ValidationError[] {
    const fullSchema: SchemaDefinition = {};
    for (const [prop, field] of Object.entries(this.tableDef.schema)) {
      let type: SchemaPrimitive = field.type;

      const hasAutoGenerate = isKeyDefinition(field) && field.options.autoGenerate === true;
      const hasSqlDefault = field.options.default !== undefined;
      const alreadyOptional = isSchemaDescriptor(type) && type.optional === true;

      if ((hasAutoGenerate || hasSqlDefault) && !alreadyOptional) {
        type = Optional(type);
      }

      fullSchema[prop] = type;
    }
    const { errors } = validateSchema(fullSchema, entity, { label: this.tableName });
    return errors;
  }

  /** Convert a database row to an entity object */
  toEntity<E = unknown>(row: Record<string, DatabaseValue>): E {
    const entity: Record<string, unknown> = {};

    for (const [prop, field] of Object.entries(this.tableDef.schema)) {
      const columnName = field.options.columnName ?? prop;

      // Try column name first, then property name (for camel transform)
      let value = row[columnName];
      if (value === undefined && columnName !== prop) {
        value = row[prop];
      }

      if (value === undefined) {
        continue;
      }

      // Apply fromDatabase transform if available, or auto-coerce Date → ISO string for DateIso columns
      if (field.options.fromDatabase) {
        entity[prop] = field.options.fromDatabase(value);
      } else if (value instanceof Date && isDateIsoType(field.type)) {
        entity[prop] = value.toISOString();
      } else if (typeof value === 'string' && isNumericType(field.type)) {
        // NUMERIC arrives as a lossless string; a column declared as a number
        // opts into JS-number semantics, so coerce it back here.
        entity[prop] = Number(value);
      } else {
        entity[prop] = value;
      }
    }

    return entity as E;
  }
}

/** Create an entity helper for a given table definition */
export const entityHelper = <T extends TableDefinition>(tableDef: T): EntityHelper<T> => new EntityHelper(tableDef);
