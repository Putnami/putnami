import { describe, expect, it } from 'bun:test';
import { Default, Int, MapOf, Optional, validateSchema } from '../../src';

describe('schema validation', () => {
  describe('validateSchema', () => {
    it('validates a correct object', () => {
      const schema = {
        name: String,
        port: Int,
      };

      const { data, errors } = validateSchema(schema, { name: 'myapp', port: 8080 });
      expect(errors).toHaveLength(0);
      expect(data.name).toBe('myapp');
      expect(data.port).toBe(8080);
    });

    it('reports errors for invalid types', () => {
      const schema = {
        port: Int,
      };

      const { errors } = validateSchema(schema, { port: 'not-a-number' });
      expect(errors.length).toBeGreaterThan(0);
    });

    it('applies default values', () => {
      const schema = {
        name: Default(String, 'test'),
        port: Default(Int, 3000),
      };

      const { data, errors } = validateSchema(schema, {});
      expect(errors).toHaveLength(0);
      expect(data.name).toBe('test');
      expect(data.port).toBe(3000);
    });

    it('allows optional fields to be missing', () => {
      const schema = {
        name: String,
        extra: Optional(String),
      };

      const { data, errors } = validateSchema(schema, { name: 'hello' });
      expect(errors).toHaveLength(0);
      expect(data.name).toBe('hello');
      expect(data.extra).toBeUndefined();
    });

    it('requires non-optional fields', () => {
      const schema = {
        name: String,
      };

      const { errors } = validateSchema(schema, {});
      expect(errors).toHaveLength(1);
      expect(errors[0].message).toContain('is required');
    });

    it('coerces string values when coerce option is set', () => {
      const schema = {
        port: Int,
      };

      const { data, errors } = validateSchema(schema, { port: '8080' }, { coerce: true });
      expect(errors).toHaveLength(0);
      expect(data.port).toBe(8080);
    });
  });

  describe('MapOf validation', () => {
    it('validates a map with string values', () => {
      const schema = { labels: MapOf(String, String) };
      const { data, errors } = validateSchema(schema, { labels: { env: 'prod', region: 'us-east' } });
      expect(errors).toHaveLength(0);
      expect(data.labels).toEqual({ env: 'prod', region: 'us-east' });
    });

    it('validates a map with number values', () => {
      const schema = { prices: MapOf(String, Number) };
      const { data, errors } = validateSchema(schema, { prices: { apple: 1.5, banana: 0.75 } });
      expect(errors).toHaveLength(0);
      expect(data.prices).toEqual({ apple: 1.5, banana: 0.75 });
    });

    it('validates a map with Int values', () => {
      const schema = { counts: MapOf(String, Int) };
      const { data, errors } = validateSchema(schema, { counts: { a: 10, b: 20 } });
      expect(errors).toHaveLength(0);
      expect(data.counts).toEqual({ a: 10, b: 20 });
    });

    it('rejects non-object values for maps', () => {
      const schema = { labels: MapOf(String, String) };
      const { errors } = validateSchema(schema, { labels: 'not-an-object' });
      expect(errors.length).toBeGreaterThan(0);
      expect(errors[0].message).toContain('must be an object');
    });

    it('rejects array values for maps', () => {
      const schema = { labels: MapOf(String, String) };
      const { errors } = validateSchema(schema, { labels: ['a', 'b'] });
      expect(errors.length).toBeGreaterThan(0);
    });

    it('validates map value types', () => {
      const schema = { counts: MapOf(String, Int) };
      const { errors } = validateSchema(schema, { counts: { a: 'not-a-number' } });
      expect(errors.length).toBeGreaterThan(0);
    });

    it('accepts empty map', () => {
      const schema = { labels: MapOf(String, String) };
      const { data, errors } = validateSchema(schema, { labels: {} });
      expect(errors).toHaveLength(0);
      expect(data.labels).toEqual({});
    });
  });
});
