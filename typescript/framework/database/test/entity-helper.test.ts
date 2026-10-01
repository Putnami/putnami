import { describe, expect, it } from 'bun:test';
import { DateIso, Email, Int, Optional, Uuid } from '@putnami/runtime';
import { entityHelper } from '../src/metadata/entity.helper';
import { Column, Key, PgArray, Table } from '../src/table';

describe('EntityHelper', () => {
  describe('tableName', () => {
    it('should return table name from definition', () => {
      const table = Table('custom_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(helper.tableName).toBe('custom_table');
    });
  });

  describe('db', () => {
    it('should return datasource name when set', () => {
      const table = Table('test_table', { id: Key(String) }, { db: 'auth' });
      const helper = entityHelper(table);
      expect(helper.db).toBe('auth');
    });

    it('should return undefined for default datasource', () => {
      const table = Table('test_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(helper.db).toBeUndefined();
    });
  });

  describe('columnName', () => {
    it('should return property name when no custom column name', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      expect(helper.columnName('name')).toBe('name');
    });

    it('should return custom column name when set', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String, { columnName: 'full_name' }),
      });
      const helper = entityHelper(table);
      expect(helper.columnName('name')).toBe('full_name');
    });

    it('should throw for unknown properties (prevents SQL identifier injection)', () => {
      const table = Table('test_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(() => helper.columnName('unknown')).toThrow(/Unknown property "unknown"/);
    });
  });

  describe('columnNames', () => {
    it('should return all column names', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
        email: Column(String),
      });
      const helper = entityHelper(table);
      const columnNames = helper.columnNames;
      expect(columnNames).toContain('id');
      expect(columnNames).toContain('name');
      expect(columnNames).toContain('email');
    });

    it('should use custom column names', () => {
      const table = Table('test_table', {
        id: Key(String),
        fullName: Column(String, { columnName: 'full_name' }),
      });
      const helper = entityHelper(table);
      expect(helper.columnNames).toContain('id');
      expect(helper.columnNames).toContain('full_name');
    });
  });

  describe('extractKeys', () => {
    it('should extract single key', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const keys = helper.extractKeys({ id: 'test-123', name: 'Test' });

      expect(keys.id).toBe('test-123');
      expect(keys.name).toBeUndefined();
    });

    it('should extract composite keys', () => {
      const table = Table('test_table', {
        userId: Key(String),
        roleId: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const keys = helper.extractKeys({
        userId: 'user-123',
        roleId: 'role-456',
        name: 'Test',
      });

      expect(keys.userId).toBe('user-123');
      expect(keys.roleId).toBe('role-456');
      expect(keys.name).toBeUndefined();
    });

    it('should handle custom key column names', () => {
      const table = Table('test_table', {
        id: Key(String, { columnName: 'entity_id' }),
      });
      const helper = entityHelper(table);
      const keys = helper.extractKeys({ id: 'test-123' });

      expect(keys.entity_id).toBe('test-123');
    });

    it('should return empty object when no keys', () => {
      // Edge case: a table with only columns (unusual but valid)
      const table = Table('test_table', {
        name: Column(String),
      });
      const helper = entityHelper(table);
      const keys = helper.extractKeys({ name: 'Test' });
      expect(keys).toEqual({});
    });
  });

  describe('keyProperties', () => {
    it('should return set of key property names', () => {
      const table = Table('test_table', {
        userId: Key(String),
        roleId: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const keyProps = helper.keyProperties;

      expect(keyProps.has('userId')).toBe(true);
      expect(keyProps.has('roleId')).toBe(true);
      expect(keyProps.has('name')).toBe(false);
    });
  });

  describe('getColumnOptions', () => {
    it('should return column options for a property', () => {
      const toDb = (val: string[]) => JSON.stringify(val);
      const table = Table('test_table', {
        id: Key(String),
        tags: Column(String, { columnType: 'JSONB', toDatabase: toDb }),
      });
      const helper = entityHelper(table);
      const opts = helper.getColumnOptions('tags');

      expect(opts?.columnType).toBe('JSONB');
      expect(opts?.toDatabase).toBe(toDb);
    });

    it('should return undefined for unknown property', () => {
      const table = Table('test_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(helper.getColumnOptions('unknown')).toBeUndefined();
    });
  });

  describe('toEntity', () => {
    it('should convert database row to entity', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
        email: Column(String),
      });
      const helper = entityHelper(table);
      const row = {
        id: 'test-123',
        name: 'Test User',
        email: 'test@example.com',
      };

      const entity = helper.toEntity(row);
      expect(entity).toEqual({
        id: 'test-123',
        name: 'Test User',
        email: 'test@example.com',
      });
    });

    it('should handle custom column names', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String, { columnName: 'full_name' }),
      });
      const helper = entityHelper(table);
      const row = {
        id: 'test-123',
        full_name: 'Test User',
      };

      const entity = helper.toEntity(row);
      expect(entity).toEqual({
        id: 'test-123',
        name: 'Test User',
      });
    });

    it('should apply fromDatabase transformation', () => {
      const table = Table('test_table', {
        id: Key(String),
        tags: Column(String, {
          fromDatabase: (value: string) => JSON.parse(value as string),
        }),
      });
      const helper = entityHelper(table);
      const row = {
        id: 'test-123',
        tags: '["tag1", "tag2"]',
      };

      const entity = helper.toEntity<{ id: string; tags: string[] }>(row);
      expect(entity.tags).toEqual(['tag1', 'tag2']);
    });

    it('should skip undefined values', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const row = { id: 'test-123' };

      const entity = helper.toEntity<{ id: string; name?: string }>(row);
      expect(entity.id).toBe('test-123');
      expect(entity.name).toBeUndefined();
    });

    it('should auto-coerce Date to ISO string for DateIso columns', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso, { columnName: 'created_at' }),
      });
      const helper = entityHelper(table);
      const date = new Date('2025-06-15T10:30:00.000Z');
      const row = { id: 'test-123', created_at: date };

      const entity = helper.toEntity<{ id: string; createdAt: string }>(row);
      expect(entity.createdAt).toBe('2025-06-15T10:30:00.000Z');
    });

    it('should auto-coerce Date to ISO string for Optional(DateIso) columns', () => {
      const table = Table('test_table', {
        id: Key(String),
        updatedAt: Column(Optional(DateIso), { columnName: 'updated_at' }),
      });
      const helper = entityHelper(table);
      const date = new Date('2025-06-15T10:30:00.000Z');
      const row = { id: 'test-123', updated_at: date };

      const entity = helper.toEntity<{ id: string; updatedAt: string }>(row);
      expect(entity.updatedAt).toBe('2025-06-15T10:30:00.000Z');
    });

    it('should not coerce Date for non-DateIso string columns', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const date = new Date('2025-06-15T10:30:00.000Z');
      const row = { id: 'test-123', name: date as any };

      const entity = helper.toEntity<{ id: string; name: unknown }>(row);
      expect(entity.name).toBeInstanceOf(Date);
    });

    it('should prefer explicit fromDatabase over auto-coercion', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso, {
          columnName: 'created_at',
          fromDatabase: (v) => (v instanceof Date ? v.toISOString().split('T')[0] : v),
        }),
      });
      const helper = entityHelper(table);
      const date = new Date('2025-06-15T10:30:00.000Z');
      const row = { id: 'test-123', created_at: date };

      const entity = helper.toEntity<{ id: string; createdAt: string }>(row);
      expect(entity.createdAt).toBe('2025-06-15');
    });

    it('should pass through string values for DateIso columns without coercion', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso, { columnName: 'created_at' }),
      });
      const helper = entityHelper(table);
      const row = { id: 'test-123', created_at: '2025-06-15T10:30:00.000Z' };

      const entity = helper.toEntity<{ id: string; createdAt: string }>(row);
      expect(entity.createdAt).toBe('2025-06-15T10:30:00.000Z');
    });

    it('should try property name as fallback for camel transform', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(String, { columnName: 'created_at' }),
      });
      const helper = entityHelper(table);
      // Simulate postgres camel transform: column "created_at" → property "createdAt"
      const row = { id: 'test-123', createdAt: '2025-01-01' };

      const entity = helper.toEntity<{ id: string; createdAt: string }>(row);
      expect(entity.createdAt).toBe('2025-01-01');
    });
  });

  describe('validateEntity', () => {
    it('should return no errors for valid entity', () => {
      const table = Table('users', {
        id: Key(Uuid),
        email: Column(Email),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const errors = helper.validateEntity({
        id: '550e8400-e29b-41d4-a716-446655440000',
        email: 'user@example.com',
        name: 'Test User',
      });
      expect(errors).toHaveLength(0);
    });

    it('should return errors for invalid Uuid', () => {
      const table = Table('users', {
        id: Key(Uuid),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const errors = helper.validateEntity({ id: 'not-a-uuid', name: 'Test' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('UUID'))).toBe(true);
    });

    it('should return errors for invalid Email', () => {
      const table = Table('users', {
        id: Key(String),
        email: Column(Email),
      });
      const helper = entityHelper(table);
      const errors = helper.validateEntity({ id: '1', email: 'not-an-email' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('email'))).toBe(true);
    });

    it('should return errors for wrong base type', () => {
      const table = Table('items', {
        id: Key(String),
        price: Column(Number),
      });
      const helper = entityHelper(table);
      const errors = helper.validateEntity({ id: '1', price: 'not-a-number' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('number'))).toBe(true);
    });

    it('should validate Int constraint', () => {
      const table = Table('items', {
        id: Key(String),
        quantity: Column(Int),
      });
      const helper = entityHelper(table);

      const noErrors = helper.validateEntity({ id: '1', quantity: 5 });
      expect(noErrors).toHaveLength(0);

      const errors = helper.validateEntity({ id: '1', quantity: 5.5 });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('integer'))).toBe(true);
    });

    it('should only validate provided fields (partial entity)', () => {
      const table = Table('users', {
        id: Key(Uuid),
        email: Column(Email),
        name: Column(String),
      });
      const helper = entityHelper(table);
      // Only validate id — email and name are not provided so not checked
      const errors = helper.validateEntity({ id: '550e8400-e29b-41d4-a716-446655440000' });
      expect(errors).toHaveLength(0);
    });

    it('should allow undefined values for Optional fields', () => {
      const table = Table('users', {
        id: Key(String),
        bio: Column(Optional(String)),
      });
      const helper = entityHelper(table);
      const errors = helper.validateEntity({ id: '1', bio: undefined });
      expect(errors).toHaveLength(0);
    });
  });

  describe('validateFullEntity', () => {
    it('should catch missing required fields', () => {
      const table = Table('users', {
        id: Key(Uuid),
        email: Column(Email),
        name: Column(String),
      });
      const helper = entityHelper(table);
      // Only provide id — email and name are missing and required
      const errors = helper.validateFullEntity({ id: '550e8400-e29b-41d4-a716-446655440000' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('email'))).toBe(true);
      expect(errors.some((e) => e.message.includes('name'))).toBe(true);
    });

    it('should not require auto-generated keys', () => {
      const table = Table('items', {
        id: Key(Uuid, { autoGenerate: true }),
        name: Column(String),
      });
      const helper = entityHelper(table);
      // id is auto-generated, so only name is required
      const errors = helper.validateFullEntity({ name: 'Test Item' });
      expect(errors).toHaveLength(0);
    });

    it('should not require columns with SQL defaults', () => {
      const table = Table('items', {
        id: Key(String),
        status: Column(String, { default: 'active' }),
        name: Column(String),
      });
      const helper = entityHelper(table);
      // status has a SQL default, so it's optional
      const errors = helper.validateFullEntity({ id: '1', name: 'Test Item' });
      expect(errors).toHaveLength(0);
    });

    it('should still respect Optional fields', () => {
      const table = Table('users', {
        id: Key(String),
        name: Column(String),
        bio: Column(Optional(String)),
      });
      const helper = entityHelper(table);
      // bio is Optional, so only id and name are required
      const errors = helper.validateFullEntity({ id: '1', name: 'Test' });
      expect(errors).toHaveLength(0);
    });

    it('should still validate provided field types', () => {
      const table = Table('users', {
        id: Key(Uuid),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const errors = helper.validateFullEntity({ id: 'not-a-uuid', name: 'Test' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors.some((e) => e.message.includes('UUID'))).toBe(true);
    });

    it('should pass when all required fields are provided', () => {
      const table = Table('users', {
        id: Key(Uuid),
        email: Column(Email),
        name: Column(String),
      });
      const helper = entityHelper(table);
      const errors = helper.validateFullEntity({
        id: '550e8400-e29b-41d4-a716-446655440000',
        email: 'user@example.com',
        name: 'Test User',
      });
      expect(errors).toHaveLength(0);
    });
  });

  describe('toDatabaseValue', () => {
    it('should auto-coerce Date to ISO string for DateIso columns', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso, { columnName: 'created_at' }),
      });
      const helper = entityHelper(table);
      const date = new Date('2025-06-15T10:30:00.000Z');
      expect(helper.toDatabaseValue('createdAt', date)).toBe('2025-06-15T10:30:00.000Z');
    });

    it('should prefer explicit toDatabase over auto-coercion', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso, {
          toDatabase: (v) => `custom:${v}`,
        }),
      });
      const helper = entityHelper(table);
      expect(helper.toDatabaseValue('createdAt', '2025-06-15')).toBe('custom:2025-06-15');
    });

    it('should pass through non-Date values for DateIso columns', () => {
      const table = Table('test_table', {
        id: Key(String),
        createdAt: Column(DateIso),
      });
      const helper = entityHelper(table);
      expect(helper.toDatabaseValue('createdAt', '2025-06-15T10:30:00.000Z')).toBe('2025-06-15T10:30:00.000Z');
    });

    it('should return value as-is for unknown properties', () => {
      const table = Table('test_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(helper.toDatabaseValue('unknown', 42)).toBe(42);
    });
  });

  describe('hasProperty', () => {
    it('should return true for known properties', () => {
      const table = Table('test_table', {
        id: Key(String),
        name: Column(String),
      });
      const helper = entityHelper(table);
      expect(helper.hasProperty('id')).toBe(true);
      expect(helper.hasProperty('name')).toBe(true);
    });

    it('should return false for unknown properties', () => {
      const table = Table('test_table', { id: Key(String) });
      const helper = entityHelper(table);
      expect(helper.hasProperty('unknown')).toBe(false);
    });
  });

  describe('PgArray columns', () => {
    it('should create a column with TEXT[] column type for String', () => {
      const table = Table('test_table', {
        id: Key(String),
        tags: PgArray(String),
      });
      const helper = entityHelper(table);
      const options = helper.getColumnOptions('tags');
      expect(options?.columnType).toBe('TEXT[]');
    });

    it('should create a column with DOUBLE PRECISION[] column type for Number', () => {
      const table = Table('test_table', {
        id: Key(String),
        scores: PgArray(Number),
      });
      const helper = entityHelper(table);
      const options = helper.getColumnOptions('scores');
      expect(options?.columnType).toBe('DOUBLE PRECISION[]');
    });

    it('should create a column with BOOLEAN[] column type for Boolean', () => {
      const table = Table('test_table', {
        id: Key(String),
        flags: PgArray(Boolean),
      });
      const helper = entityHelper(table);
      const options = helper.getColumnOptions('flags');
      expect(options?.columnType).toBe('BOOLEAN[]');
    });

    it('should apply toDatabase transform', () => {
      const table = Table('test_table', {
        id: Key(String),
        tags: PgArray(String),
      });
      const helper = entityHelper(table);
      expect(helper.toDatabaseValue('tags', ['a', 'b'])).toEqual(['a', 'b']);
      expect(helper.toDatabaseValue('tags', undefined)).toEqual([]);
    });

    it('should apply fromDatabase transform', () => {
      const table = Table('test_table', {
        id: Key(String),
        tags: PgArray(String, { columnName: 'tag_list' }),
      });
      const helper = entityHelper(table);
      const entity = helper.toEntity<{ id: string; tags: string[] }>({
        id: 'test-1',
        tag_list: ['x', 'y'],
      });
      expect(entity.tags).toEqual(['x', 'y']);
    });

    it('should support custom column name', () => {
      const table = Table('test_table', {
        id: Key(String),
        grantTypes: PgArray(String, { columnName: 'grant_types' }),
      });
      const helper = entityHelper(table);
      expect(helper.columnName('grantTypes')).toBe('grant_types');
    });
  });
});
