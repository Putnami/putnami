import { describe, expect, it } from 'bun:test';
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
  baseTypeName,
  isNestedSchema,
  isSchemaDescriptor,
} from '../../../src/api/route';

describe('Schema', () => {
  describe('isSchemaDescriptor', () => {
    it('should return true for schema descriptors', () => {
      expect(isSchemaDescriptor(Uuid)).toBe(true);
      expect(isSchemaDescriptor(Email)).toBe(true);
      expect(isSchemaDescriptor(Int)).toBe(true);
      expect(isSchemaDescriptor(Optional(String))).toBe(true);
      expect(isSchemaDescriptor(ArrayOf(String))).toBe(true);
    });

    it('should return false for JS constructors and non-schema values', () => {
      expect(isSchemaDescriptor(String)).toBe(false);
      expect(isSchemaDescriptor(Number)).toBe(false);
      expect(isSchemaDescriptor(Boolean)).toBe(false);
      expect(isSchemaDescriptor(null)).toBe(false);
      expect(isSchemaDescriptor(undefined)).toBe(false);
      expect(isSchemaDescriptor('hello')).toBe(false);
      expect(isSchemaDescriptor(42)).toBe(false);
    });
  });

  describe('baseTypeName', () => {
    it('should return correct type names for JS constructors', () => {
      expect(baseTypeName(String)).toBe('string');
      expect(baseTypeName(Number)).toBe('number');
      expect(baseTypeName(Boolean)).toBe('boolean');
    });

    it('should return correct type names for schema descriptors', () => {
      expect(baseTypeName(Uuid)).toBe('string');
      expect(baseTypeName(Email)).toBe('string');
      expect(baseTypeName(Int)).toBe('number');
      expect(baseTypeName(ArrayOf(String))).toBe('array');
    });
  });

  describe('Optional', () => {
    it('should create an optional schema descriptor from a JS constructor', () => {
      const opt = Optional(String);
      expect(isSchemaDescriptor(opt)).toBe(true);
      expect(opt.optional).toBe(true);
      expect(opt.baseType).toBe('string');
    });

    it('should create an optional schema descriptor from another descriptor', () => {
      const opt = Optional(Uuid);
      expect(isSchemaDescriptor(opt)).toBe(true);
      expect(opt.optional).toBe(true);
      expect(opt.baseType).toBe('string');
      expect(opt.constraints?.length).toBe(1);
    });

    it('should preserve constraints from nested descriptor', () => {
      const opt = Optional(Int);
      expect(opt.optional).toBe(true);
      expect(opt.constraints?.[0]?.name).toBe('integer');
    });
  });

  describe('ArrayOf', () => {
    it('should create an array schema descriptor', () => {
      const arr = ArrayOf(String);
      expect(isSchemaDescriptor(arr)).toBe(true);
      expect(arr.array).toBe(true);
      expect(arr.baseType).toBe('array');
      expect(arr.items).toBe(String);
    });

    it('should support constrained item types', () => {
      const arr = ArrayOf(Uuid);
      expect(arr.array).toBe(true);
      expect(isSchemaDescriptor(arr.items)).toBe(true);
    });
  });

  describe('Constrained types', () => {
    describe('Uuid', () => {
      it('should validate correct UUIDs', () => {
        const validate = Uuid.constraints?.[0].validate;
        expect(validate('550e8400-e29b-41d4-a716-446655440000')).toBe(true);
        expect(validate('6ba7b810-9dad-11d1-80b4-00c04fd430c8')).toBe(true);
      });

      it('should reject invalid UUIDs', () => {
        const validate = Uuid.constraints?.[0].validate;
        expect(validate('not-a-uuid')).toBe(false);
        expect(validate('')).toBe(false);
        expect(validate(123)).toBe(false);
      });
    });

    describe('Email', () => {
      it('should validate correct emails', () => {
        const validate = Email.constraints?.[0].validate;
        expect(validate('test@example.com')).toBe(true);
        expect(validate('user.name@domain.org')).toBe(true);
      });

      it('should reject invalid emails', () => {
        const validate = Email.constraints?.[0].validate;
        expect(validate('not-an-email')).toBe(false);
        expect(validate('@missing.com')).toBe(false);
        expect(validate(42)).toBe(false);
      });
    });

    describe('Int', () => {
      it('should validate integers', () => {
        const validate = Int.constraints?.[0].validate;
        expect(validate(42)).toBe(true);
        expect(validate(0)).toBe(true);
        expect(validate(-10)).toBe(true);
      });

      it('should reject non-integers', () => {
        const validate = Int.constraints?.[0].validate;
        expect(validate(3.14)).toBe(false);
        expect(validate('42')).toBe(false);
      });
    });

    describe('Url', () => {
      it('should validate correct URLs', () => {
        const validate = Url.constraints?.[0].validate;
        expect(validate('https://example.com')).toBe(true);
        expect(validate('http://example.com/path?q=1')).toBe(true);
        expect(validate('ftp://files.example.com')).toBe(true);
      });

      it('should reject invalid URLs', () => {
        const validate = Url.constraints?.[0].validate;
        expect(validate('not-a-url')).toBe(false);
        expect(validate('')).toBe(false);
        expect(validate(42)).toBe(false);
      });
    });

    describe('DateIso', () => {
      it('should validate ISO date strings', () => {
        const validate = DateIso.constraints?.[0].validate;
        expect(validate('2024-01-15')).toBe(true);
        expect(validate('2024-01-15T10:30:00Z')).toBe(true);
        expect(validate('2024-01-15T10:30:00.000+05:00')).toBe(true);
      });

      it('should reject invalid dates', () => {
        const validate = DateIso.constraints?.[0].validate;
        expect(validate('not-a-date')).toBe(false);
        expect(validate('15/01/2024')).toBe(false);
        expect(validate(42)).toBe(false);
      });
    });

    describe('Min', () => {
      it('should validate values >= minimum', () => {
        const min5 = Min(5);
        const validate = min5.constraints?.[0].validate;
        expect(validate(5)).toBe(true);
        expect(validate(10)).toBe(true);
      });

      it('should reject values below minimum', () => {
        const min5 = Min(5);
        const validate = min5.constraints?.[0].validate;
        expect(validate(4)).toBe(false);
        expect(validate(-1)).toBe(false);
      });
    });

    describe('Max', () => {
      it('should validate values <= maximum', () => {
        const max10 = Max(10);
        const validate = max10.constraints?.[0].validate;
        expect(validate(10)).toBe(true);
        expect(validate(5)).toBe(true);
      });

      it('should reject values above maximum', () => {
        const max10 = Max(10);
        const validate = max10.constraints?.[0].validate;
        expect(validate(11)).toBe(false);
        expect(validate(100)).toBe(false);
      });
    });

    describe('MinLength', () => {
      it('should validate strings with length >= minimum', () => {
        const minLen3 = MinLength(3);
        const validate = minLen3.constraints?.[0].validate;
        expect(validate('abc')).toBe(true);
        expect(validate('abcdef')).toBe(true);
      });

      it('should reject strings shorter than minimum', () => {
        const minLen3 = MinLength(3);
        const validate = minLen3.constraints?.[0].validate;
        expect(validate('ab')).toBe(false);
        expect(validate('')).toBe(false);
      });
    });

    describe('MaxLength', () => {
      it('should validate strings with length <= maximum', () => {
        const maxLen5 = MaxLength(5);
        const validate = maxLen5.constraints?.[0].validate;
        expect(validate('abc')).toBe(true);
        expect(validate('abcde')).toBe(true);
      });

      it('should reject strings longer than maximum', () => {
        const maxLen5 = MaxLength(5);
        const validate = maxLen5.constraints?.[0].validate;
        expect(validate('abcdef')).toBe(false);
      });
    });

    describe('Pattern', () => {
      it('should validate strings matching the pattern', () => {
        const hex = Pattern(/^[0-9a-f]+$/);
        const validate = hex.constraints?.[0].validate;
        expect(validate('deadbeef')).toBe(true);
        expect(validate('0123456789abcdef')).toBe(true);
      });

      it('should reject strings not matching the pattern', () => {
        const hex = Pattern(/^[0-9a-f]+$/);
        const validate = hex.constraints?.[0].validate;
        expect(validate('xyz')).toBe(false);
        expect(validate('DEADBEEF')).toBe(false);
      });
    });
  });

  describe('isNestedSchema', () => {
    it('should detect nested object schemas', () => {
      expect(isNestedSchema({ street: String, city: String })).toBe(true);
      expect(isNestedSchema({ name: String, tags: ArrayOf(String) })).toBe(true);
    });

    it('should reject non-nested values', () => {
      expect(isNestedSchema(String)).toBe(false);
      expect(isNestedSchema(Uuid)).toBe(false);
      expect(isNestedSchema(null)).toBe(false);
      expect(isNestedSchema(undefined)).toBe(false);
      expect(isNestedSchema(42)).toBe(false);
    });
  });
});
