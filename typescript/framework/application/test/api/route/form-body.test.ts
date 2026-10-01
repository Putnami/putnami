import { describe, expect, it } from 'bun:test';
import { http } from '../../../src/http/http.plugin';
import { api } from '../../../src/api/api.plugin';
import { endpoint, isEndpointDefinition } from '../../../src/api/route/endpoint';
import { Email, Int, Optional } from '../../../src/api/route';
import { application } from '../../../src/application';

describe('endpoint().body() with contentType option', () => {
  describe('builder', () => {
    it('should create an EndpointDefinition with bodyContentType', () => {
      const def = endpoint()
        .body({ grant_type: String }, { contentType: 'application/x-www-form-urlencoded' })
        .handle(async (ctx) => {
          const body = await ctx.body();
          return { grant_type: body.grant_type };
        });
      expect(isEndpointDefinition(def)).toBe(true);
      expect(def.schemas?.body).toBeDefined();
      expect(def.schemas?.bodyContentType).toBe('application/x-www-form-urlencoded');
    });

    it('should default bodyContentType to undefined for JSON bodies', () => {
      const def = endpoint()
        .body({ name: String })
        .handle(async (ctx) => await ctx.body());
      expect(def.schemas?.bodyContentType).toBeUndefined();
    });
  });

  describe('integration', () => {
    it('should parse and validate form-urlencoded body', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body(
          { grant_type: String, code: Optional(String), redirect_uri: Optional(String) },
          { contentType: 'application/x-www-form-urlencoded' },
        )
        .handle(async (ctx) => {
          const body = await ctx.body();
          return { grant_type: body.grant_type, code: body.code };
        });

      plugin.register('/token', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/token`, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body: 'grant_type=authorization_code&code=abc123',
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.grant_type).toBe('authorization_code');
      expect(data.code).toBe('abc123');

      await app.stop();
    });

    it('should coerce form values to numbers', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ name: String, age: Int }, { contentType: 'application/x-www-form-urlencoded' })
        .handle(async (ctx) => {
          const body = await ctx.body();
          return { name: body.name, age: body.age, ageType: typeof body.age };
        });

      plugin.register('/submit', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body: 'name=John&age=25',
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.name).toBe('John');
      expect(data.age).toBe(25);
      expect(data.ageType).toBe('number');

      await app.stop();
    });

    it('should return 400 for invalid form-urlencoded body', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ grant_type: String, email: Email }, { contentType: 'application/x-www-form-urlencoded' })
        .handle(async (ctx) => await ctx.body());

      plugin.register('/submit', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/submit`, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body: 'grant_type=test&email=invalid',
      });
      expect(res.status).toBe(400);

      await app.stop();
    });

    it('should handle missing required form fields', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ grant_type: String, code: String }, { contentType: 'application/x-www-form-urlencoded' })
        .handle(async (ctx) => await ctx.body());

      plugin.register('/token', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/token`, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body: 'grant_type=authorization_code',
      });
      expect(res.status).toBe(400);

      await app.stop();
    });
  });
});
