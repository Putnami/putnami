import { describe, expect, it } from 'bun:test';
import { Optional, Uuid } from '@putnami/runtime';
import { Column, Key, Table } from '../src/table';
import { isColumnDefinition, isKeyDefinition, isTableDefinition } from '../src/table/table-definition';

describe('Table builders', () => {
  describe('Column()', () => {
    it('should create a column definition with marker', () => {
      const col = Column(String);
      expect(isColumnDefinition(col)).toBe(true);
      expect(isKeyDefinition(col)).toBe(false);
      expect(col.type).toBe(String);
      expect(col.options).toEqual({});
    });

    it('should accept column options', () => {
      const col = Column(String, { columnName: 'full_name', columnType: 'VARCHAR(255)' });
      expect(col.options.columnName).toBe('full_name');
      expect(col.options.columnType).toBe('VARCHAR(255)');
    });

    it('should accept default value', () => {
      const col = Column(String, { default: 'active' });
      expect(col.options.default).toBe('active');
    });

    it('should accept transform functions', () => {
      const toDb = (val: string[]) => JSON.stringify(val);
      const fromDb = (val: string) => JSON.parse(val);
      const col = Column(String, { toDatabase: toDb, fromDatabase: fromDb });
      expect(col.options.toDatabase).toBe(toDb);
      expect(col.options.fromDatabase).toBe(fromDb);
    });

    it('should accept schema descriptors', () => {
      const col = Column(Uuid);
      expect(isColumnDefinition(col)).toBe(true);
    });

    it('should accept Optional schema descriptors', () => {
      const col = Column(Optional(String));
      expect(isColumnDefinition(col)).toBe(true);
    });
  });

  describe('Key()', () => {
    it('should create a key definition with marker', () => {
      const key = Key(Uuid);
      expect(isKeyDefinition(key)).toBe(true);
      expect(isColumnDefinition(key)).toBe(false);
    });

    it('should accept autoGenerate option', () => {
      const key = Key(Uuid, { autoGenerate: true });
      expect(key.options.autoGenerate).toBe(true);
    });

    it('should accept custom column name', () => {
      const key = Key(Uuid, { columnName: 'entity_id' });
      expect(key.options.columnName).toBe('entity_id');
    });
  });

  describe('Table()', () => {
    it('should create a table definition with marker', () => {
      const table = Table('users', {
        id: Key(String),
        name: Column(String),
      });
      expect(isTableDefinition(table)).toBe(true);
      expect(table.tableName).toBe('users');
    });

    it('should set named datasource', () => {
      const table = Table('users', { id: Key(String) }, { db: 'auth' });
      expect(table.options.db).toBe('auth');
    });

    it('should set schema option', () => {
      const table = Table('users', { id: Key(String) }, { schema: 'public' });
      expect(table.options.schema).toBe('public');
    });

    // Migration registration on Table() was removed in the migration
    // redesign — migrations now live in feature plugins via
    // app.MigrationContributor + database.sqlSourceInline, not on the
    // Table builder. See typescript/framework/database/src/migrations/
    // for the new shape.

    it('should preserve schema structure', () => {
      const table = Table('products', {
        id: Key(Uuid, { autoGenerate: true }),
        name: Column(String, { columnName: 'product_name' }),
        price: Column(Number, { columnType: 'DECIMAL(10,2)' }),
        description: Column(Optional(String)),
      });

      expect(isKeyDefinition(table.schema.id)).toBe(true);
      expect(isColumnDefinition(table.schema.name)).toBe(true);
      expect(isColumnDefinition(table.schema.price)).toBe(true);
      expect(isColumnDefinition(table.schema.description)).toBe(true);
    });
  });

  describe('Type guards', () => {
    it('isColumnDefinition should reject non-column values', () => {
      expect(isColumnDefinition(null)).toBe(false);
      expect(isColumnDefinition(undefined)).toBe(false);
      expect(isColumnDefinition({})).toBe(false);
      expect(isColumnDefinition('string')).toBe(false);
      expect(isColumnDefinition(Key(String))).toBe(false);
    });

    it('isKeyDefinition should reject non-key values', () => {
      expect(isKeyDefinition(null)).toBe(false);
      expect(isKeyDefinition(undefined)).toBe(false);
      expect(isKeyDefinition({})).toBe(false);
      expect(isKeyDefinition(Column(String))).toBe(false);
    });

    it('isTableDefinition should reject non-table values', () => {
      expect(isTableDefinition(null)).toBe(false);
      expect(isTableDefinition(undefined)).toBe(false);
      expect(isTableDefinition({})).toBe(false);
      expect(isTableDefinition(Column(String))).toBe(false);
    });
  });
});
