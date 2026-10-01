import { describe, expect, it } from 'bun:test';
import { negotiateResponse, negotiateType, parseAccept } from '../../src/http/content-negotiation';
import { HttpResponse } from '../../src/http/http-response';

describe('content negotiation', () => {
  describe('parseAccept', () => {
    it('should return wildcard for null header', () => {
      const result = parseAccept(null);
      expect(result).toEqual([{ type: '*/*', quality: 1 }]);
    });

    it('should parse a simple Accept header', () => {
      const result = parseAccept('application/json');
      expect(result).toHaveLength(1);
      expect(result[0].type).toBe('application/json');
      expect(result[0].quality).toBe(1);
    });

    it('should parse multiple types with quality values', () => {
      const result = parseAccept('text/html, application/json;q=0.9, text/plain;q=0.8');
      expect(result).toHaveLength(3);
      expect(result[0].type).toBe('text/html');
      expect(result[0].quality).toBe(1);
      expect(result[1].type).toBe('application/json');
      expect(result[1].quality).toBe(0.9);
      expect(result[2].type).toBe('text/plain');
      expect(result[2].quality).toBe(0.8);
    });

    it('should sort by quality descending', () => {
      const result = parseAccept('text/plain;q=0.5, application/json;q=0.9, text/html');
      expect(result[0].type).toBe('text/html');
      expect(result[1].type).toBe('application/json');
      expect(result[2].type).toBe('text/plain');
    });
  });

  describe('negotiateType', () => {
    it('should match exact type', () => {
      const accepted = parseAccept('application/json');
      expect(negotiateType(accepted, ['application/json', 'text/plain'])).toBe('application/json');
    });

    it('should match wildcard', () => {
      const accepted = parseAccept('*/*');
      expect(negotiateType(accepted, ['application/json'])).toBe('application/json');
    });

    it('should return undefined when no match', () => {
      const accepted = parseAccept('image/png');
      expect(negotiateType(accepted, ['application/json', 'text/plain'])).toBeUndefined();
    });

    it('should prefer higher quality matches', () => {
      const accepted = parseAccept('text/plain;q=0.5, application/json;q=0.9');
      expect(negotiateType(accepted, ['text/plain', 'application/json'])).toBe('application/json');
    });
  });

  describe('negotiateResponse', () => {
    it('should return JSON for application/json', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'application/json');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toBe('application/json');
    });

    it('should return JSON for wildcard accept', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, '*/*');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toBe('application/json');
    });

    it('should return JSON for null accept header', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, null);
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toBe('application/json');
    });

    it('should return plain text for text/plain', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'text/plain');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('text/plain');
    });

    it('should return HTML for text/html', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'text/html');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('text/html');
    });

    it('should return XML for application/xml', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'application/xml');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('application/xml');
    });

    it('should return XML for text/xml', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'text/xml');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('text/xml');
    });

    it('sanitizes object keys into valid XML tag names', async () => {
      const data = { 'bad key</x>': 'v', '1leading': 'n', xmlReserved: 'r' };
      const response = negotiateResponse(data, 'application/xml');
      const body = await response.get().text();
      // No raw injection from the malicious key, and the value is intact.
      expect(body).not.toContain('bad key</x>');
      expect(body).toContain('<bad_key__x_>v</bad_key__x_>');
      // Keys starting with a digit or the reserved `xml` prefix are defused.
      expect(body).toContain('<_1leading>n</_1leading>');
      expect(body).toContain('<_xmlReserved>r</_xmlReserved>');
    });

    it('should return YAML for application/yaml', () => {
      const data = { name: 'John', age: 30 };
      const response = negotiateResponse(data, 'application/yaml');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('application/yaml');
    });

    it('should return YAML for text/yaml', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'text/yaml');
      expect(response).toBeInstanceOf(HttpResponse);
      expect(response.getHeader('Content-Type')).toContain('text/yaml');
    });

    it('should serialize nested objects to YAML', async () => {
      const data = { user: { name: 'John', tags: ['admin', 'active'] } };
      const response = negotiateResponse(data, 'application/yaml');
      const body = await response.get().text();
      expect(body).toContain('user:');
      expect(body).toContain('name: John');
      expect(body).toContain('- admin');
      expect(body).toContain('- active');
    });

    it('should quote special YAML strings', async () => {
      const data = { value: 'true', empty: '', num: '42' };
      const response = negotiateResponse(data, 'application/yaml');
      const body = await response.get().text();
      expect(body).toContain('value: "true"');
      expect(body).toContain('empty: ""');
      expect(body).toContain('num: "42"');
    });

    it('should serialize null and empty structures in YAML', async () => {
      const data = { nothing: null, items: [] };
      const response = negotiateResponse(data, 'application/yaml');
      const body = await response.get().text();
      expect(body).toContain('nothing: null');
      expect(body).toContain('items: []');
    });

    it('should throw NotAcceptableException for unsupported types', () => {
      const data = { name: 'John' };
      expect(() => negotiateResponse(data, 'image/png')).toThrow();
    });

    it('should prefer JSON when multiple types include json', () => {
      const data = { name: 'John' };
      const response = negotiateResponse(data, 'text/html;q=0.5, application/json;q=0.9');
      expect(response.getHeader('Content-Type')).toBe('application/json');
    });
  });
});
