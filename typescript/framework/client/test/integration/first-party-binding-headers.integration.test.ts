import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import {
  application,
  api,
  authenticate,
  endpoint,
  extractBearerToken,
  http,
  openapi,
  PrincipalKind,
} from '@putnami/application';
import { resetConfigLoader, runInContext } from '@putnami/runtime';
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

/**
 * Provider declaration → generation → loaded emitted client → real bound
 * calls, the TypeScript twin of the Go openapi `client_binding_headers_test`.
 * The provider declares a forwarded-user credential; the generated consumer
 * binds a static advisory header beside it, once programmatically and once
 * through the shared `clients.services.<id>.headers` configuration, and every
 * call carries the same revision while forwarding its own user.
 *
 * The only double is the identity resolver, which recognizes two fixed bearer
 * tokens in place of an external issuer. Provider, generator, emitted client,
 * HTTP runtime and socket calls are real.
 */
describe('first-party static binding headers', () => {
  specTest(
    'the emitted client carries the binding headers beside each forwarded user',
    {
      feature: 'typescript/service-clients',
      requirement: 'binding-headers',
      check: 'the-emitted-typescript-client-carries-binding-headers-beside-forwarded-users',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      providerHttp.prepend(
        authenticate({
          anyOf: [
            (context) => {
              const token = extractBearerToken(context);
              return token === 'first-user' || token === 'second-user'
                ? { sub: token, kind: PrincipalKind.User }
                : undefined;
            },
          ],
        }),
      );
      const providerApi = api({
        autoScan: false,
        client: {
          service: { id: 'revision-provider', audience: 'revision-provider' },
          credentials: { user: { kind: 'forwarded-user-token' } },
        },
      });
      providerApi.register(
        '/revision',
        endpoint()
          .returns({ revision: String, caller: String })
          .secure({ principalKind: 'user' })
          .client({ security: { alternatives: [{ allOf: [{ profile: 'user' }] }] }, idempotency: { kind: 'safe' } })
          .handle((context) => ({
            revision: context.req.headers.get('X-Putnami-Observed-Revision') ?? '',
            caller: context.user?.sub ?? '',
          })),
        'GET',
      );
      const providerOpenApi = openapi({ title: 'Revisions', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      const consumers: ReturnType<typeof application>[] = [];
      const disposables: { dispose(): void }[] = [];

      await provider.start();
      try {
        const port = providerHttp.getServer()?.port;
        const contract = providerOpenApi.spec();
        if (!port || !contract) throw new Error('provider did not start or publish its contract');
        const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
        const revisionMethod = ir.services.flatMap((service) => service.methods).find((m) => m.path === '/revision');
        if (!revisionMethod) throw new Error('the revision operation did not reach the generated IR');

        const files = generateTypeScriptClient(ir, { packageName: '@test/revisions-binding-headers-client' });
        expect(files.every((file) => !file.content.includes('revision-one'))).toBe(true);
        const directory = mkdtempSync(join(tmpdir(), 'putnami-binding-headers-client-'));
        temporaryDirectories.push(directory);
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
        const registerName = Object.keys(generated).find((name) => /^register\w+Client$/.test(name));
        const bindName = Object.keys(generated).find((name) => /^bind\w+Client$/.test(name));
        const className = registerName?.replace(/^register/, '');
        if (!registerName || !bindName || !className || typeof generated[className] !== 'function') {
          throw new Error(`the generated package exports no client: ${Object.keys(generated).join(', ')}`);
        }
        type Reply = { revision: string; caller: string };
        type Generated = Record<string, () => Promise<Reply>> & { dispose(): void };
        const Client = generated[className] as new (...args: never[]) => Generated;
        const register = generated[registerName] as (
          target: ReturnType<typeof application>,
          binding?: Record<string, unknown>,
        ) => unknown;
        const bind = generated[bindName] as (binding: Record<string, unknown>) => Generated;
        const asUser = (user: string, client: Generated) =>
          runInContext({ __authorizationHeader: `Bearer ${user}` }, () => client[revisionMethod.name]?.call(client));

        // 1. A programmatic binding, through the generated bind helper. The
        //    caller's map is mutated after binding and that changes nothing.
        const headers: Record<string, string> = { 'X-Putnami-Observed-Revision': 'revision-one' };
        const bound = bind({
          url: `http://localhost:${port}`,
          clientId: 'revision-consumer',
          allowInsecure: true,
          headers,
          credentials: { user: { source: 'forwarded-user' } },
        });
        disposables.push(bound);
        headers['X-Putnami-Observed-Revision'] = 'mutated';
        expect(await asUser('first-user', bound)).toEqual({ revision: 'revision-one', caller: 'first-user' });
        expect(await asUser('second-user', bound)).toEqual({ revision: 'revision-one', caller: 'second-user' });

        // 2. The same deployment configuration a Go consumer would load,
        //    resolved through config and DI by the generated register helper.
        process.env.CONFIG_DATA = JSON.stringify({
          clients: {
            clientId: 'revision-consumer',
            services: {
              'revision-provider': {
                url: `http://localhost:${port}`,
                allowInsecure: true,
                headers: { 'X-Putnami-Observed-Revision': 'revision-one' },
                credentials: { user: { source: 'forwarded-user' } },
              },
            },
          },
        });
        resetConfigLoader();
        const consumer = application();
        consumers.push(consumer);
        register(consumer);
        await consumer.start();
        const configured = consumer.context.get(Client);
        expect(await asUser('first-user', configured)).toEqual({ revision: 'revision-one', caller: 'first-user' });
        expect(await asUser('second-user', configured)).toEqual({ revision: 'revision-one', caller: 'second-user' });
      } finally {
        for (const disposable of disposables) disposable.dispose();
        for (const consumer of consumers) await consumer.stop();
        await provider.stop();
      }
    },
  );
});
