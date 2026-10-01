import { describe, expect, it } from 'bun:test';
import type { AotValidators } from '@putnami/runtime';
import { compileSchemaValidator, Int } from '@putnami/runtime';
import { api } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route/endpoint';
import { application } from '../../../src/application';
import { http } from '../../../src/http/http.plugin';

// biome-ignore lint/security/noGlobalEval: materialising generated validator source under test (mirrors codegen output)
const materialize = (source: string) => eval(`(${source})`) as AotValidators['query'];

describe('AOT-validated dispatch (build-time codegen output)', () => {
  it('behaves identically to the generic validator for a query endpoint', async () => {
    const querySchema = { page: Int, limit: Int };
    const ep = {
      GET: endpoint()
        .query(querySchema)
        .handle((ctx) => ({ q: ctx.queryParams() })),
    };
    const aot: AotValidators = { query: materialize(compileSchemaValidator(querySchema, { coerce: true }).source) };

    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });
    plugin.register('/aot', ep, 'GET', aot); // AOT validators (as codegen would emit)
    plugin.register('/gen', ep, 'GET'); // generic, for parity
    const app = application().use(httpPlugin).use(plugin);
    await app.start();
    const base = `http://localhost:${httpPlugin.getServer()?.port}`;

    // Valid input: both coerce strings to integers identically.
    for (const path of ['/aot', '/gen']) {
      const res = await fetch(`${base}${path}?page=2&limit=20`);
      expect(res.status).toBe(200);
      expect(await res.json()).toEqual({ q: { page: 2, limit: 20 } });
    }

    // Invalid input: the AOT path falls back to the generic validator → same 400.
    for (const path of ['/aot', '/gen']) {
      const res = await fetch(`${base}${path}?page=abc&limit=20`);
      expect(res.status).toBe(400);
      await res.arrayBuffer();
    }
    // Non-integer → also 400 via fallback.
    const nonInt = await fetch(`${base}/aot?page=2.5&limit=1`);
    expect(nonInt.status).toBe(400);
    await nonInt.arrayBuffer();

    await app.stop();
  });

  it('does not apply AOT when the endpoint has middleware', async () => {
    // An endpoint with route middleware keeps the generic handler (codegen omits AOT for these).
    const ep = {
      GET: endpoint()
        .query({ page: Int })
        .cors()
        .handle((ctx) => ({ q: ctx.queryParams() })),
    };
    const aot: AotValidators = { query: materialize(compileSchemaValidator({ page: Int }, { coerce: true }).source) };

    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });
    plugin.register('/mw', ep, 'GET', aot);
    const app = application().use(httpPlugin).use(plugin);
    await app.start();
    const base = `http://localhost:${httpPlugin.getServer()?.port}`;

    // Still works (generic path) — validation and coercion intact.
    const res = await fetch(`${base}/mw?page=5`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ q: { page: 5 } });

    await app.stop();
  });
});
