import { describe, expect, it } from 'bun:test';
import { BadRequestException } from '@putnami/runtime';
import {
  ArrayOf,
  DateIso,
  Email,
  Int,
  Max,
  MaxLength,
  Min,
  MinLength,
  Optional,
  Pattern,
  Url,
  Uuid,
} from '../../../src/api/route';
import { validateSchema } from '../../../src/api/route/validate';

describe('validateSchema', () => {
  describe('required fields', () => {
    it('should validate required string fields', () => {
      const result = validateSchema({ name: String }, { name: 'John' });
      expect(result).toEqual({ name: 'John' });
    });

    it('should throw on missing required fields', () => {
      expect(() => validateSchema({ name: String }, {})).toThrow(BadRequestException);
    });

    it('should throw on null required fields', () => {
      expect(() => validateSchema({ name: String }, { name: null })).toThrow(BadRequestException);
    });
  });

  describe('optional fields', () => {
    it('should allow missing optional fields', () => {
      const result = validateSchema({ name: Optional(String) }, {});
      expect(result).toEqual({});
    });

    it('should validate present optional fields', () => {
      const result = validateSchema({ name: Optional(String) }, { name: 'John' });
      expect(result).toEqual({ name: 'John' });
    });
  });

  describe('type checking', () => {
    it('should validate string type', () => {
      expect(() => validateSchema({ name: String }, { name: 42 })).toThrow(BadRequestException);
    });

    it('should validate number type', () => {
      const result = validateSchema({ age: Number }, { age: 25 });
      expect(result).toEqual({ age: 25 });
    });

    it('should reject wrong number type', () => {
      expect(() => validateSchema({ age: Number }, { age: 'twenty' })).toThrow(BadRequestException);
    });

    it('should validate boolean type', () => {
      const result = validateSchema({ active: Boolean }, { active: true });
      expect(result).toEqual({ active: true });
    });

    it('should reject wrong boolean type', () => {
      expect(() => validateSchema({ active: Boolean }, { active: 'yes' })).toThrow(BadRequestException);
    });
  });

  describe('coercion', () => {
    it('should coerce string to number when coerce is enabled', () => {
      const result = validateSchema({ age: Number }, { age: '25' }, { coerce: true });
      expect(result).toEqual({ age: 25 });
    });

    it('should coerce string to boolean when coerce is enabled', () => {
      const result = validateSchema({ active: Boolean }, { active: 'true' }, { coerce: true });
      expect(result).toEqual({ active: true });
    });

    it('should coerce "false" string to false', () => {
      const result = validateSchema({ active: Boolean }, { active: 'false' }, { coerce: true });
      expect(result).toEqual({ active: false });
    });

    it('should not coerce when disabled', () => {
      expect(() => validateSchema({ age: Number }, { age: '25' }, { coerce: false })).toThrow(BadRequestException);
    });

    it('should fail coercion for non-numeric strings', () => {
      expect(() => validateSchema({ age: Number }, { age: 'abc' }, { coerce: true })).toThrow(BadRequestException);
    });
  });

  describe('constraints', () => {
    it('should validate Uuid constraint', () => {
      const result = validateSchema({ id: Uuid }, { id: '550e8400-e29b-41d4-a716-446655440000' });
      expect(result.id).toBe('550e8400-e29b-41d4-a716-446655440000');
    });

    it('should reject invalid Uuid', () => {
      expect(() => validateSchema({ id: Uuid }, { id: 'not-a-uuid' })).toThrow(BadRequestException);
    });

    it('should validate Email constraint', () => {
      const result = validateSchema({ email: Email }, { email: 'test@example.com' });
      expect(result.email).toBe('test@example.com');
    });

    it('should reject invalid Email', () => {
      expect(() => validateSchema({ email: Email }, { email: 'invalid' })).toThrow(BadRequestException);
    });

    it('should validate Int constraint', () => {
      const result = validateSchema({ count: Int }, { count: 42 });
      expect(result.count).toBe(42);
    });

    it('should reject non-integer for Int', () => {
      expect(() => validateSchema({ count: Int }, { count: 3.14 })).toThrow(BadRequestException);
    });
  });

  describe('arrays', () => {
    it('should validate array of strings', () => {
      const result = validateSchema({ tags: ArrayOf(String) }, { tags: ['a', 'b', 'c'] });
      expect(result).toEqual({ tags: ['a', 'b', 'c'] });
    });

    it('should reject non-array', () => {
      expect(() => validateSchema({ tags: ArrayOf(String) }, { tags: 'not-array' })).toThrow(BadRequestException);
    });

    it('should reject array with wrong item types', () => {
      expect(() => validateSchema({ tags: ArrayOf(String) }, { tags: [1, 2, 3] })).toThrow(BadRequestException);
    });

    it('should validate array of constrained types', () => {
      const ids = ['550e8400-e29b-41d4-a716-446655440000', '6ba7b810-9dad-11d1-80b4-00c04fd430c8'];
      const result = validateSchema({ ids: ArrayOf(Uuid) }, { ids });
      expect(result.ids).toEqual(ids);
    });
  });

  describe('multiple fields', () => {
    it('should validate a complex schema', () => {
      const schema = {
        name: String,
        email: Email,
        age: Number,
        active: Boolean,
        tags: ArrayOf(String),
        nickname: Optional(String),
      };
      const input = {
        name: 'John',
        email: 'john@example.com',
        age: 30,
        active: true,
        tags: ['admin'],
      };
      const result = validateSchema(schema, input);
      expect(result).toEqual(input);
    });

    it('should collect all errors', () => {
      const schema = { name: String, email: Email };
      try {
        validateSchema(schema, {});
        expect(true).toBe(false); // should not reach
      } catch (e) {
        expect(e).toBeInstanceOf(BadRequestException);
      }
    });

    it('should include structured errors in response', () => {
      const schema = { name: String, email: Email };
      try {
        validateSchema(schema, {}, { label: 'body' });
        expect(true).toBe(false);
      } catch (e) {
        const error = e as BadRequestException;
        const response = error.getResponse() as {
          message?: string;
          errors?: Array<{ field: string; message: string }>;
        };

        expect(response.message).toBe('body.name is required; body.email is required');
        expect(response.errors).toEqual([
          { field: 'body.name', message: 'body.name is required' },
          { field: 'body.email', message: 'body.email is required' },
        ]);
      }
    });
  });

  describe('new constrained types', () => {
    it('should validate Url constraint', () => {
      const result = validateSchema({ link: Url }, { link: 'https://example.com' });
      expect(result.link).toBe('https://example.com');
    });

    it('should reject invalid Url', () => {
      expect(() => validateSchema({ link: Url }, { link: 'not-a-url' })).toThrow(BadRequestException);
    });

    it('should validate DateIso constraint', () => {
      const result = validateSchema({ date: DateIso }, { date: '2024-01-15T10:30:00Z' });
      expect(result.date).toBe('2024-01-15T10:30:00Z');
    });

    it('should reject invalid DateIso', () => {
      expect(() => validateSchema({ date: DateIso }, { date: '15/01/2024' })).toThrow(BadRequestException);
    });

    it('should validate Min constraint', () => {
      const result = validateSchema({ age: Min(18) }, { age: 25 });
      expect(result.age).toBe(25);
    });

    it('should reject below Min', () => {
      expect(() => validateSchema({ age: Min(18) }, { age: 10 })).toThrow(BadRequestException);
    });

    it('should validate Max constraint', () => {
      const result = validateSchema({ score: Max(100) }, { score: 85 });
      expect(result.score).toBe(85);
    });

    it('should reject above Max', () => {
      expect(() => validateSchema({ score: Max(100) }, { score: 150 })).toThrow(BadRequestException);
    });

    it('should validate MinLength constraint', () => {
      const result = validateSchema({ name: MinLength(2) }, { name: 'Jo' });
      expect(result.name).toBe('Jo');
    });

    it('should reject below MinLength', () => {
      expect(() => validateSchema({ name: MinLength(2) }, { name: 'J' })).toThrow(BadRequestException);
    });

    it('should validate MaxLength constraint', () => {
      const result = validateSchema({ code: MaxLength(5) }, { code: 'ABC' });
      expect(result.code).toBe('ABC');
    });

    it('should reject above MaxLength', () => {
      expect(() => validateSchema({ code: MaxLength(5) }, { code: 'ABCDEF' })).toThrow(BadRequestException);
    });

    it('should validate Pattern constraint', () => {
      const result = validateSchema({ slug: Pattern(/^[a-z0-9-]+$/) }, { slug: 'my-post-123' });
      expect(result.slug).toBe('my-post-123');
    });

    it('should reject non-matching Pattern', () => {
      expect(() => validateSchema({ slug: Pattern(/^[a-z0-9-]+$/) }, { slug: 'My Post!' })).toThrow(
        BadRequestException,
      );
    });
  });

  describe('nested object schemas', () => {
    it('should validate a nested object', () => {
      const schema = {
        name: String,
        address: { street: String, city: String },
      };
      const input = {
        name: 'John',
        address: { street: '123 Main St', city: 'Springfield' },
      };
      const result = validateSchema(schema, input);
      expect(result).toEqual(input);
    });

    it('should reject non-object for nested schema', () => {
      const schema = { address: { street: String, city: String } };
      expect(() => validateSchema(schema, { address: 'not-an-object' })).toThrow(BadRequestException);
    });

    it('should reject missing required fields in nested object', () => {
      const schema = { address: { street: String, city: String } };
      expect(() => validateSchema(schema, { address: { street: '123 Main St' } })).toThrow(BadRequestException);
    });

    it('should support optional fields in nested objects', () => {
      const schema = {
        address: { street: String, zip: Optional(String) },
      };
      const result = validateSchema(schema, { address: { street: '123 Main St' } });
      expect(result).toEqual({ address: { street: '123 Main St' } });
    });

    it('should support deeply nested objects', () => {
      const schema = {
        user: {
          name: String,
          location: { city: String, country: String },
        },
      };
      const input = {
        user: {
          name: 'John',
          location: { city: 'Paris', country: 'France' },
        },
      };
      const result = validateSchema(schema, input);
      expect(result).toEqual(input);
    });

    it('should validate constraints within nested objects', () => {
      const schema = {
        contact: { email: Email, website: Optional(Url) },
      };
      const result = validateSchema(schema, {
        contact: { email: 'john@example.com', website: 'https://example.com' },
      });
      expect(result).toEqual({
        contact: { email: 'john@example.com', website: 'https://example.com' },
      });
    });

    it('should reject invalid constraints in nested objects', () => {
      const schema = {
        contact: { email: Email },
      };
      expect(() => validateSchema(schema, { contact: { email: 'invalid' } })).toThrow(BadRequestException);
    });
  });

  describe('error labels', () => {
    it('should include label in error messages', () => {
      try {
        validateSchema({ id: String }, {}, { label: 'params' });
        expect(true).toBe(false);
      } catch (e) {
        expect((e as BadRequestException).message).toContain('params.id');
      }
    });
  });
});
