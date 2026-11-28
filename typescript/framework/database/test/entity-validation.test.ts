import { describe, expect, it } from 'bun:test';
import {
  ArrayOf,
  DateIso,
  Default,
  Email,
  Int,
  MaxLength,
  Min,
  Optional,
  type SchemaDefinition,
  type SchemaPrimitive,
  Uuid,
  validateSchema,
} from '@putnami/runtime';
import { Column, EntityHelper, isKeyDefinition, Key, Table } from '../src/index';

/**
 * A save validates its entity with checks compiled once per table, and an
 * entity they do not accept is validated as it always was: `validateEntity`
 * and `validateFullEntity` report what `validateSchema` reports on the schema
 * they always built, for any entity, in the same order.
 */

const Users = Table('validated_users', {
  id: Key(Uuid),
  email: Column(Email),
  name: Column(MaxLength(20)),
  age: Column(Min(0)),
  rank: Column(Int),
  nickname: Column(Optional(String)),
  score: Column(Default(Number, 1)),
  active: Column(Boolean),
  createdAt: Column(DateIso, { columnName: 'created_at', default: 'NOW()' }),
  token: Key(String, { autoGenerate: true }),
});

/** A table with a field no check compiles: its saves are validated as before. */
const Tagged = Table('validated_tagged', {
  id: Key(String),
  tags: Column(ArrayOf(String)),
  label: Column(String),
});

/** The errors of the partial validation, as `validateEntity` built them before any compiled check. */
function partialReference(table: typeof Users | typeof Tagged, entity: Record<string, unknown>) {
  const definition: SchemaDefinition = {};
  for (const [prop, field] of Object.entries(table.schema)) definition[prop] = field.type;
  const partial: SchemaDefinition = {};
  for (const key of Object.keys(entity)) {
    if (key in definition) partial[key] = definition[key] as SchemaPrimitive;
  }
  return validateSchema(partial, entity, { label: table.tableName }).errors;
}

/** The errors of the full validation, as `validateFullEntity` built them before any compiled check. */
function fullReference(table: typeof Users | typeof Tagged, entity: Record<string, unknown>) {
  const full: SchemaDefinition = {};
  for (const [prop, field] of Object.entries(table.schema)) {
    const optional =
      ((isKeyDefinition(field) && field.options.autoGenerate === true) || field.options.default !== undefined) &&
      !(typeof field.type === 'object' && (field.type as { optional?: boolean }).optional === true);
    full[prop] = optional ? Optional(field.type) : field.type;
  }
  return validateSchema(full, entity, { label: table.tableName }).errors;
}

const ID = '7c9e6679-7425-40de-944b-e07fc1f70f00';

const userEntities: Record<string, unknown>[] = [
  { id: ID },
  { id: ID, email: 'a@b.co', name: 'Ada', age: 36, active: true },
  { id: ID, email: 'a@b.co', name: 'Ada', age: 36, active: true, nickname: null, score: undefined },
  { id: ID, email: 'a@b.co', name: 'Ada', age: 36, active: true, createdAt: '2026-01-02T03:04:05.000Z', token: 't' },
  { id: 'not-a-uuid' },
  { id: ID, email: 'nope' },
  { id: ID, name: 'x'.repeat(21) },
  { id: ID, name: 'x'.repeat(20) },
  { id: ID, age: -1 },
  { id: ID, age: 1.5 },
  { id: ID, rank: 1.5 },
  { id: ID, rank: 2 },
  { id: ID, age: '3' },
  { id: ID, age: Number.NaN },
  { id: ID, active: 'true' },
  { id: ID, active: null },
  { id: ID, email: undefined },
  { id: ID, nickname: 3 },
  { id: ID, score: null },
  { id: ID, score: 'high' },
  { id: ID, createdAt: 'yesterday' },
  { id: ID, createdAt: new Date('2026-01-02T03:04:05.000Z') },
  { id: ID, email: 'nope', age: -1, active: 'no', name: 7 },
  { id: ID, bogus: 1, other: null },
  { id: ID, constructor: undefined },
  { id: ID, toString: 'x' },
  { id: null, email: 'a@b.co' },
  JSON.parse(`{"id":"${ID}","__proto__":{"admin":true}}`),
];

describe('the validation of a save', () => {
  it('reports what validateSchema reports on the partial schema, for any entity', () => {
    const helper = new EntityHelper(Users);
    for (const entity of userEntities) {
      // Twice: the first call builds the compiled checks, the second uses them.
      expect(helper.validateEntity(entity)).toEqual(partialReference(Users, entity));
      expect(helper.validateEntity(entity)).toEqual(partialReference(Users, entity));
    }
  });

  it('reports what validateSchema reports on the full schema, for any entity', () => {
    const helper = new EntityHelper(Users);
    for (const entity of userEntities) {
      expect(helper.validateFullEntity(entity)).toEqual(fullReference(Users, entity));
      expect(helper.validateFullEntity(entity)).toEqual(fullReference(Users, entity));
    }
  });

  it('validates a field no check compiles as before, partial and full', () => {
    const helper = new EntityHelper(Tagged);
    const entities: Record<string, unknown>[] = [
      { id: 'a', tags: ['x'], label: 'l' },
      { id: 'a', tags: 'x' },
      { id: 'a', tags: [1, 'x'] },
      { id: 'a', label: 3 },
      { id: 'a', label: 'l' },
      { id: 'a' },
    ];
    for (const entity of entities) {
      expect(helper.validateEntity(entity)).toEqual(partialReference(Tagged, entity));
      expect(helper.validateFullEntity(entity)).toEqual(fullReference(Tagged, entity));
    }
  });

  it('hands out a new error list on each call', () => {
    const helper = new EntityHelper(Users);
    const first = helper.validateEntity({ id: ID });
    first.push({ field: 'x', message: 'y' });
    expect(helper.validateEntity({ id: ID })).toEqual([]);
    const strict = helper.validateFullEntity({ id: ID, email: 'a@b.co', name: 'Ada', age: 1, rank: 1, active: true });
    strict.push({ field: 'x', message: 'y' });
    expect(helper.validateFullEntity({ id: ID, email: 'a@b.co', name: 'Ada', age: 1, rank: 1, active: true })).toEqual(
      [],
    );
  });
});
