import { afterAll, afterEach, beforeAll, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import type { OpenApiDocument } from '@putnami/application';
import { readOpenApiSpec } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

// End-to-end Phase-2a proof: an OpenAPI spec with $ref-shared models flows
// OpenAPI → IR → emitted client, and the *emitted* client (not a hand-written
// stand-in) compiles, instantiates, and round-trips against a live server with
// fully typed request/response shapes — no blanket Record<string, unknown>.

// Absolute path to this package's entry. The client is emitted to an out-of-tree
// temp dir, so its `@putnami/client` import is rewritten to this path; its own
// transitive imports then resolve from the real package location.
const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');

// A spec exercising the typed-IR features: a top-level $ref response and body
// (Widget), a $ref nested inside a model (dimensions), an array of $ref (none
// here — tags is a scalar array), an array of $ref in an inline response
// (listWidgets.widgets), and path params.
const openApiDoc: OpenApiDocument = {
  openapi: '3.0.3',
  info: { title: 'Widgets API', version: '1.0.0' },
  paths: {
    '/widgets/{id}': {
      get: {
        operationId: 'getWidget',
        parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
        responses: {
          '200': {
            description: 'OK',
            content: { 'application/json': { schema: { $ref: '#/components/schemas/Widget' } } },
          },
        },
      },
    },
    '/widgets': {
      get: {
        operationId: 'listWidgets',
        responses: {
          '200': {
            description: 'OK',
            content: {
              'application/json': {
                schema: {
                  type: 'object',
                  properties: { widgets: { type: 'array', items: { $ref: '#/components/schemas/Widget' } } },
                  required: ['widgets'],
                },
              },
            },
          },
        },
      },
      post: {
        operationId: 'createWidget',
        requestBody: {
          required: true,
          content: { 'application/json': { schema: { $ref: '#/components/schemas/Widget' } } },
        },
        responses: {
          '201': {
            description: 'Created',
            content: { 'application/json': { schema: { $ref: '#/components/schemas/Widget' } } },
          },
        },
      },
    },
  },
  components: {
    schemas: {
      Widget: {
        type: 'object',
        properties: {
          id: { type: 'string' },
          name: { type: 'string' },
          weightGrams: { type: 'number' },
          dimensions: { $ref: '#/components/schemas/Dimensions' },
          tags: { type: 'array', items: { type: 'string' } },
        },
        required: ['id', 'name', 'weightGrams'],
      },
      Dimensions: {
        type: 'object',
        properties: { width: { type: 'number' }, height: { type: 'number' } },
        required: ['width', 'height'],
      },
    },
  },
};

// Emit the client into a temp package and return the import-ready entry path plus
// the raw source of types.ts / the client file for source-level assertions.
function emitClient(): { indexPath: string; typesSource: string; clientSource: string; outDir: string } {
  const spec = readOpenApiSpec(openApiDoc);
  const files = generateTypeScriptClient(spec, { packageName: '@test/widgets-client' });

  const outDir = mkdtempSync(join(tmpdir(), 'putnami-clientgen-'));
  let typesSource = '';
  let clientSource = '';
  for (const file of files) {
    // Rewrite the package import so the out-of-tree client resolves the real
    // runtime; relative imports (./types, ./widgets-client) resolve in-place.
    const content = file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`);
    const dest = join(outDir, file.path);
    mkdirSync(dirname(dest), { recursive: true });
    writeFileSync(dest, content);
    if (file.path === 'src/types.ts') typesSource = file.content;
    if (file.path === 'src/widgets-client.ts') clientSource = file.content;
  }
  return { indexPath: join(outDir, 'src', 'index.ts'), typesSource, clientSource, outDir };
}

// ---------------------------------------------------------------------------
// Test server returning schema-matching JSON
// ---------------------------------------------------------------------------

const WIDGET = {
  id: 'w1',
  name: 'Sprocket',
  weightGrams: 42,
  dimensions: { width: 3, height: 4 },
  tags: ['metal', 'round'],
};

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === '/widgets' && req.method === 'GET') {
        return Response.json({ widgets: [WIDGET] });
      }
      if (url.pathname === '/widgets' && req.method === 'POST') {
        const body = await req.json();
        return Response.json(body, { status: 201 });
      }
      if (url.pathname.startsWith('/widgets/') && req.method === 'GET') {
        const id = url.pathname.split('/')[2];
        return Response.json({ ...WIDGET, id });
      }
      return new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

const tmpDirs: string[] = [];
afterEach(() => {
  for (const dir of tmpDirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe('generated TS client (typed, end-to-end)', () => {
  test('emits typed models from $ref schemas — no blanket Record', () => {
    const { typesSource, clientSource } = emitClient();

    // Shared models surface as real interfaces…
    expect(typesSource).toContain('export interface Widget {');
    expect(typesSource).toContain('export interface Dimensions {');
    expect(typesSource).toContain('weightGrams: number;');
    expect(typesSource).toContain('dimensions?: Dimensions;');
    expect(typesSource).toContain('tags?: string[];');
    // …and the inline list response references the model, not a degraded map.
    expect(typesSource).toContain('widgets: Widget[];');
    expect(typesSource).not.toContain('Record<string, unknown>');

    // Method signatures use the named models directly.
    expect(clientSource).toContain('async getWidget(params: GetWidgetParams): Promise<Widget>');
    expect(clientSource).toContain('async createWidget(body: Widget): Promise<Widget>');
    expect(clientSource).toContain('async listWidgets(): Promise<ListWidgetsResponse>');
  });

  test('the emitted client round-trips against a live server', async () => {
    const { indexPath, outDir } = emitClient();
    tmpDirs.push(outDir);

    const mod = await import(indexPath);
    const WidgetsClient = mod.WidgetsClient as new (config: {
      baseUrl: string;
      transport: 'http';
      timeoutMs: number;
    }) => {
      getWidget(params: { id: string }): Promise<typeof WIDGET>;
      listWidgets(): Promise<{ widgets: (typeof WIDGET)[] }>;
      createWidget(body: typeof WIDGET): Promise<typeof WIDGET>;
    };

    const client = new WidgetsClient({ baseUrl, transport: 'http', timeoutMs: 5000 });

    // GET with a path param → typed model back.
    const got = await client.getWidget({ id: 'w9' });
    expect(got.id).toBe('w9');
    expect(got.name).toBe('Sprocket');
    expect(got.dimensions.width).toBe(3);
    expect(got.tags).toEqual(['metal', 'round']);

    // GET list → inline response wrapping an array of the model.
    const list = await client.listWidgets();
    expect(list.widgets).toHaveLength(1);
    expect(list.widgets[0].weightGrams).toBe(42);

    // POST with a typed body → echoed back.
    const created = await client.createWidget({ ...WIDGET, id: 'w2', name: 'Cog' });
    expect(created.id).toBe('w2');
    expect(created.name).toBe('Cog');
  });
});
