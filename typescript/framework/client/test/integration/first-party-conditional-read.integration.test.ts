import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, HttpResponse, http, openapi } from '@putnami/application';
import { resetConfigLoader, type Type } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

type SummaryResult =
  | { status: 200; data: { count: number }; headers: Headers }
  | { status: 304; data: undefined; headers: Headers };

describe('first-party conditional read, provider to generated consumer', () => {
  specTest(
    'the emitted client sends If-None-Match, reads the ETag of a 200 and answers a 304 as a result',
    {
      feature: 'typescript/service-clients',
      requirement: 'conditional-read',
      check: 'a-declared-304-is-a-typed-result-of-a-conditional-read',
    },
    async () => {
      let count = 3;
      const received: (string | null)[] = [];
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'missions', audience: 'urn:missions' }, credentials: {} },
      });
      providerApi.register(
        '/summary',
        endpoint()
          .returns({ count: Number })
          .response(304, 'If-None-Match names the current ETag: no body.')
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
          .handle((context) => {
            const condition = context.headers.get('If-None-Match');
            received.push(condition);
            const etag = `"v${count}"`;
            if (condition === etag) return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
            return HttpResponse.json({ count }, { headers: { ETag: etag } });
          }),
        'GET',
      );
      const providerOpenApi = openapi({ title: 'Missions', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      const consumer = application();

      await provider.start();
      try {
        const port = providerHttp.getServer()?.port;
        const contract = providerOpenApi.spec();
        if (!port || !contract) throw new Error('provider did not start');
        // Provider declaration → published contract → strict reader → emitter.
        const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
        const method = ir.services.flatMap((service) => service.methods).find((entry) => entry.path === '/summary');
        expect(method?.notModified?.description).toBe('If-None-Match names the current ETag: no body.');
        const files = generateTypeScriptClient(ir, { packageName: '@test/missions-client' });
        const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
        expect(types).toContain('ifNoneMatch?: string;');
        expect(types).toContain('status: 304; data: void; headers: Headers');

        const directory = mkdtempSync(join(tmpdir(), 'putnami-conditional-client-'));
        temporaryDirectories.push(directory);
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
        const clientName = Object.keys(generated).find(
          (name) => name.endsWith('Client') && !name.startsWith('register') && !name.startsWith('bind'),
        );
        const registerName = Object.keys(generated).find((name) => name.startsWith('register'));
        if (!clientName || !registerName || !method) throw new Error('the emitted module has no client binding');
        const Client = generated[clientName] as Type<Record<string, unknown>>;
        (generated[registerName] as (target: typeof consumer) => unknown)(consumer);
        process.env.CONFIG_DATA = JSON.stringify({
          clients: {
            clientId: 'consumer-tests',
            services: { missions: { url: `http://localhost:${port}`, allowInsecure: true } },
          },
        });
        resetConfigLoader();
        await consumer.start();

        const client = consumer.context.get(Client);
        const summary = (options?: { ifNoneMatch?: string }) =>
          (client[method.name] as (options?: { ifNoneMatch?: string }) => Promise<SummaryResult>).call(client, options);

        // The first read has no ETag to name: a 200 whose headers carry one.
        const first = await summary();
        expect(first.status).toBe(200);
        expect(first.data).toEqual({ count: 3 });
        const etag = first.headers.get('ETag');
        expect(etag).toBe('"v3"');

        // Naming it while the summary is unchanged: a 304, not an error.
        const unchanged = await summary({ ifNoneMatch: etag ?? undefined });
        expect(unchanged.status).toBe(304);
        expect(unchanged.data).toBeUndefined();

        // Once it changed, the same condition reads the new body and its ETag.
        count = 4;
        const changed = await summary({ ifNoneMatch: etag ?? undefined });
        expect(changed.status).toBe(200);
        expect(changed.data).toEqual({ count: 4 });
        expect(changed.headers.get('ETag')).toBe('"v4"');

        // A call without the option sends no If-None-Match at all.
        expect(received).toEqual([null, '"v3"', '"v3"']);
      } finally {
        await consumer.stop();
        await provider.stop();
      }
    },
  );
});
