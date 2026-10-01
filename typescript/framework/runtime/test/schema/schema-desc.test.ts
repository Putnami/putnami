import { describe, expect, it } from 'bun:test';
import { Desc, Email, Int, Optional, Uuid, validateSchema } from '../../src';

describe('Desc()', () => {
  it('should add description to String', () => {
    const field = Desc('Full name', String);
    expect(field.description).toBe('Full name');
    expect(field.baseType).toBe('string');
  });

  it('should add description to a SchemaDescriptor', () => {
    const field = Desc('User email', Email);
    expect(field.description).toBe('User email');
    expect(field.baseType).toBe('string');
    expect(field.constraints).toBeDefined();
  });

  it('should add description to Optional()', () => {
    const field = Desc('Optional age', Optional(Int));
    expect(field.description).toBe('Optional age');
    expect(field.optional).toBe(true);
  });

  it('should wrap a nested object schema and preserve its shape', () => {
    const field = Desc('Profile data', { age: Number, name: String });
    expect(field.description).toBe('Profile data');
    expect(field.baseType).toBe('object');
    // The nested schema rides on the descriptor so the config extractor
    // and validator can recurse without losing the inner fields.
    expect(field.schema).toEqual({ age: Number, name: String });
  });

  it('should validate values against a Desc-wrapped nested schema', () => {
    const schema = {
      profile: Desc('Profile data', { age: Int, name: String }),
    };
    const { data, errors } = validateSchema(schema, { profile: { age: 30, name: 'Alice' } });
    expect(errors).toHaveLength(0);
    expect(data.profile).toEqual({ age: 30, name: 'Alice' });
  });

  it('should not break validation when used on scalar fields', () => {
    const schema = {
      name: Desc('Full name', String),
      age: Desc('Age in years', Int),
      email: Desc('Contact email', Optional(Email)),
    };

    const { data, errors } = validateSchema(schema, {
      name: 'Alice',
      age: 30,
      email: 'alice@example.com',
    });

    expect(errors).toHaveLength(0);
    expect(data.name).toBe('Alice');
    expect(data.age).toBe(30);
    expect(data.email).toBe('alice@example.com');
  });

  it('should validate type correctly with Desc() wrapping', () => {
    const schema = {
      id: Desc('Unique identifier', Uuid),
    };

    const { errors } = validateSchema(schema, { id: 'not-a-uuid' });
    expect(errors.length).toBeGreaterThan(0);
  });
});
