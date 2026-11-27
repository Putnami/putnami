import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, HttpResponse, http, openapi } from '@putnami/application';
import { resetConfigLoader, type Type } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { ClientError } from '../../src';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'redirect-success';
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;
const servers: { stop: (force?: boolean) => unknown }[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  for (const server of servers.splice(0)) server.stop(true);
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

type Call = (input: { path: { id: string } }) => Promise<{ status: number; location?: string; data: unknown }>;

/**
 * Provider declaration → published contract → strict reader → emitter →
 * loaded client → real call. `/artifacts/{id}` declares a 302; `/artifacts/{id}/plain`
 * declares none. `answer` decides what each one actually sends.
 */
async function withRedirectingProvider(
  answer: (path: string, id: string) => Response,
  run: (calls: { open: Call; plain: Call }, followed: () => number) => Promise<void>,
): Promise<void> {
  let hits = 0;
  const target = Bun.serve({
    port: 0,
    fetch: () => {
      hits++;
      return new Response('followed');
    },
  });
  servers.push(target);
  const targetUrl = `http://localhost:${target.port}`;
  const providerHttp = http({ port: 0 });
  const providerApi = api({
    autoScan: false,
    client: { service: { id: 'artifacts', audience: 'urn:artifacts' }, credentials: {} },
  });
  const client = { security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } } as const;
  providerApi.register(
    '/artifacts/[id]',
    endpoint()
      .params({ id: String })
      .response(302, "The artifact's location")
      .client(client)
      .handle((context) => {
        const id = context.params.id;
        return toHttpResponse(answer('/artifacts', id), targetUrl, id);
      }),
    'GET',
  );
  providerApi.register(
    '/artifacts/[id]/plain',
    endpoint()
      .params({ id: String })
      .returns({ id: String })
      .client(client)
      .handle((context) => {
        const id = context.params.id;
        return toHttpResponse(answer('/plain', id), targetUrl, id);
      }),
    'GET',
  );
  const providerOpenApi = openapi({ title: 'Artifacts', version: '1.0.0' });
  const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  const consumer = application();
  await provider.start();
  try {
    const port = providerHttp.getServer()?.port;
    const contract = providerOpenApi.spec();
    if (!port || !contract) throw new Error('provider did not start');
    const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
    const methods = ir.services.flatMap((service) => service.methods);
    const open = methods.find((entry) => !entry.path.endsWith('/plain'));
    const plain = methods.find((entry) => entry.path.endsWith('/plain'));
    if (!open || !plain) throw new Error('the reader left an operation out');
    expect(open.successes?.some((success) => success.status === 302)).toBe(true);

    const directory = mkdtempSync(join(tmpdir(), 'putnami-redirect-client-'));
    temporaryDirectories.push(directory);
    for (const file of generateTypeScriptClient(ir, { packageName: '@test/artifacts-client' })) {
      const destination = join(directory, file.path);
      mkdirSync(dirname(destination), { recursive: true });
      writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
    }
    const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
    const clientName = Object.keys(generated).find(
      (name) => name.endsWith('Client') && !name.startsWith('register') && !name.startsWith('bind'),
    );
    const registerName = Object.keys(generated).find((name) => name.startsWith('register'));
    if (!clientName || !registerName) throw new Error('the emitted module has no client binding');
    const Client = generated[clientName] as Type<Record<string, unknown>>;
    (generated[registerName] as (target: typeof consumer) => unknown)(consumer);
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'consumer-tests',
        services: { artifacts: { url: `http://localhost:${port}`, allowInsecure: true } },
      },
    });
    resetConfigLoader();
    await consumer.start();
    const instance = consumer.context.get(Client);
    const bound = (name: string): Call => (instance[name] as Call).bind(instance);
    await run({ open: bound(open.name), plain: bound(plain.name) }, () => hits);
  } finally {
    await consumer.stop();
    await provider.stop();
  }
}

/** Stand for a provider answer: a redirect toward the target, or a plain body. */
function toHttpResponse(response: Response, targetUrl: string, id: string): HttpResponse {
  const location = response.headers.get('Location');
  const headers: Record<string, string> = {};
  if (location !== null) headers['Location'] = location.replace('{target}', targetUrl).replace('{id}', id);
  if (response.status >= 300 && response.status < 400) {
    return new HttpResponse('<a href="moved">Found</a>.', {
      status: response.status,
      headers: { ...headers, 'Content-Type': 'text/html; charset=utf-8' },
    });
  }
  return HttpResponse.json({ id }, { status: response.status, headers });
}

const redirectTo = (status: number) => new Response(null, { status, headers: { Location: '{target}/blobs/{id}' } });

describe('first-party redirect, provider to generated consumer', () => {
  specTest(
    'the emitted client answers a declared 302 with its status and Location, and never follows it',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-declared-redirect-is-answered-with-its-status-and-location-without-following-it',
    },
    async () => {
      await withRedirectingProvider(
        (_path, id) => (id === 'nowhere' ? new Response(null, { status: 302 }) : redirectTo(302)),
        async ({ open }, followed) => {
          const answer = await open({ path: { id: 'a1' } });
          expect(answer.status).toBe(302);
          expect(answer.location).toMatch(/^http:\/\/localhost:\d+\/blobs\/a1$/);
          expect(answer.data).toBeUndefined();
          // A declared redirect without a Location answers an empty one, as in Go.
          const nowhere = await open({ path: { id: 'nowhere' } });
          expect(nowhere.status).toBe(302);
          expect(nowhere.location).toBe('');
          expect(followed()).toBe(0);
        },
      );
    },
  );

  specTest(
    'refuses a redirect the operation does not declare',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-undeclared-redirect-is-still-refused' },
    async () => {
      await withRedirectingProvider(
        (path, _id) => {
          if (path === '/plain') return redirectTo(302);
          return redirectTo(307);
        },
        async ({ open, plain }, followed) => {
          // An operation that declares no redirect refuses every one.
          await expect(plain({ path: { id: 'a1' } })).rejects.toBeInstanceOf(ClientError);
          // A redirect status the operation does not declare is refused.
          await expect(open({ path: { id: 'a1' } })).rejects.toThrow(/undeclared success status/);
          expect(followed()).toBe(0);
        },
      );
    },
  );
});
