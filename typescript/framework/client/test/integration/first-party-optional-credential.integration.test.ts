import { afterEach, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { apiKeyStrategy, application, api, authenticate, endpoint, http, openapi } from '@putnami/application';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

/**
 * Provider declaration → generation → loaded emitted client → real bound call,
 * for an optional credential: a public package read that answers more to a
 * caller holding the registry key (ADR 0002 of go/framework/security). The
 * generated client presents the key when its binding holds one and calls
 * anonymously when it does not, instead of failing with a credential error.
 */
describe('first-party optional credential', () => {
  test('the emitted client presents the credential it holds and calls anonymously otherwise', async () => {
    const presented: (string | null)[] = [];
    const providerHttp = http({ port: 0 });
    providerHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: ['registry-key'] })] }));
    const providerApi = api({
      autoScan: false,
      client: {
        service: { id: 'registry.packages', audience: 'api://packages' },
        credentials: { 'registry-key': { kind: 'api-key', header: 'X-Api-Key' } },
      },
    });
    providerApi.register(
      '/packages/[name]/resolve',
      endpoint()
        .params({ name: String })
        .returns({ name: String, caller: String })
        .secure({ optional: true })
        .client({
          security: { alternatives: [{ allOf: [{ profile: 'registry-key' }] }, { allOf: [] }] },
          idempotency: { kind: 'safe' },
        })
        .handle((context) => {
          presented.push(context.req.headers.get('X-Api-Key'));
          return { name: context.params.name, caller: context.user ? 'key-holder' : 'anonymous' };
        }),
      'GET',
    );
    const providerOpenApi = openapi({ title: 'Packages', version: '1.0.0' });
    const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
    const consumers: ReturnType<typeof application>[] = [];

    await provider.start();
    try {
      const port = providerHttp.getServer()?.port;
      const contract = providerOpenApi.spec();
      if (!port || !contract) throw new Error('provider did not start or publish its contract');
      const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
      const resolveMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/packages/{name}/resolve');
      if (!resolveMethod) throw new Error('the resolve operation did not reach the generated IR');
      expect(resolveMethod.client?.security.alternatives).toEqual([
        { allOf: [{ profile: 'registry-key' }] },
        { allOf: [] },
      ]);

      const files = generateTypeScriptClient(ir, { packageName: '@test/packages-optional-credential-client' });
      const directory = mkdtempSync(join(tmpdir(), 'putnami-optional-credential-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }
      const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
      const registerName = Object.keys(generated).find((name) => /^register\w+Client$/.test(name));
      const className = registerName?.replace(/^register/, '');
      if (!registerName || !className || typeof generated[className] !== 'function')
        throw new Error(`the generated package exports no client: ${Object.keys(generated).join(', ')}`);
      type Generated = Record<string, (input: { path: { name: string } }) => Promise<{ name: string; caller: string }>>;
      const Client = generated[className] as new (...args: never[]) => Generated;
      const register = generated[registerName] as (
        target: ReturnType<typeof application>,
        binding: Record<string, unknown>,
      ) => unknown;

      const call = async (credentials?: Record<string, unknown>) => {
        const consumer = application();
        consumers.push(consumer);
        register(consumer, {
          url: `http://localhost:${port}`,
          clientId: 'registry.cli',
          allowInsecure: true,
          ...(credentials ? { credentials } : {}),
        });
        await consumer.start();
        const client = consumer.context.get(Client);
        return client[resolveMethod.name].call(client, { path: { name: 'private' } });
      };

      expect(await call({ 'registry-key': { source: 'static', value: 'registry-key' } })).toEqual({
        name: 'private',
        caller: 'key-holder',
      });
      expect(await call()).toEqual({ name: 'private', caller: 'anonymous' });
      // The provider saw the key once and nothing on the anonymous call: the
      // consumer never wrote a header, the declared alternatives decided.
      expect(presented).toEqual(['registry-key', null]);
    } finally {
      for (const consumer of consumers) await consumer.stop();
      await provider.stop();
    }
  });
});
