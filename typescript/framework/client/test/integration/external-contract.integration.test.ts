import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, grpc, http, openapi, proto } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

/**
 * TypeScript provider → generated TypeScript consumer, for a provider that
 * serves a standard protocol beside its own routes (clientcontract ADR 0011).
 *
 * The registry below serves an OCI Distribution Specification leg and a
 * Putnami route on one API. The document it publishes keeps both; the client
 * the emitter produces from it has the Putnami route alone, and that client
 * calls it over a real socket.
 */

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const OCI = 'OCI Distribution Specification v1.1';
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

describe('a provider that serves a standard protocol beside its own routes', () => {
  specTest(
    'publishes both, and the generated client exposes and calls only its own',
    {
      feature: 'typescript/service-clients',
      requirement: 'external-contract-operations',
      check: 'the-emitted-typescript-client-exposes-only-the-first-party-routes-through-a-real-provider',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: {
          service: { id: 'oci-server', audience: 'oci-server' },
          credentials: { user: { kind: 'forwarded-user-token' } },
        },
      });
      providerApi.register(
        '/v2/[name]/manifests/[reference]',
        endpoint()
          .params({ name: String, reference: String })
          .client({ external: OCI })
          .handle((context) => ({ schemaVersion: 2, name: context.params.name })),
        'GET',
      );
      providerApi.register(
        '/v2/_putnami/capabilities',
        endpoint()
          .returns({ copy: Boolean, revert: Boolean })
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
          .handle(() => ({ copy: true, revert: false })),
        'GET',
      );
      const providerOpenApi = openapi({ title: 'Registry', version: '1.0.0' });
      const provider = application()
        .use(providerHttp)
        .use(providerApi)
        .use(proto({ packageName: 'registry.v1' }))
        .use(grpc())
        .use(providerOpenApi);

      await provider.start();
      try {
        const server = providerHttp.getServer();
        if (!server) throw new Error('provider HTTP server did not start');
        const document = providerOpenApi.spec();
        if (!document) throw new Error('provider OpenAPI contract was not emitted');
        expect(document.paths['/v2/{name}/manifests/{reference}']?.get['x-putnami-external-contract']).toBe(OCI);

        const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
        const methods = ir.services.flatMap((service) => service.methods);
        expect(methods.map((method) => method.path)).toEqual(['/v2/_putnami/capabilities']);

        const files = generateTypeScriptClient(ir, { packageName: '@test/registry-client' });
        for (const file of files) {
          expect(file.content, file.path).not.toContain('manifests');
          expect(file.content, file.path).not.toContain(OCI);
        }
        const directory = mkdtempSync(join(tmpdir(), 'putnami-external-contract-client-'));
        temporaryDirectories.push(directory);
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
        expect(ir.services).toHaveLength(1);
        const ClientClass = generated[ir.services[0].className] as new (config: {
          baseUrl: string;
          transport: 'http';
        }) => Record<string, (...args: never[]) => unknown>;
        const client = new ClientClass({ baseUrl: `http://localhost:${server.port}`, transport: 'http' });

        const capabilities = client[methods[0].name] as () => Promise<{ copy: boolean; revert: boolean }>;
        expect(await capabilities.call(client)).toEqual({ copy: true, revert: false });

        // The provider still serves the standard leg over REST, and offers no
        // Connect RPC for it: the descriptor declares none.
        const manifest = await fetch(`http://localhost:${server.port}/v2/library/manifests/latest`);
        expect(manifest.status).toBe(200);
        expect(await manifest.json()).toEqual({ schemaVersion: 2, name: 'library' });
        const services = document['x-putnami-client']?.protobuf?.services ?? [];
        expect(services.flatMap((service) => service.methods.map((method) => method.name))).toHaveLength(1);
      } finally {
        await provider.stop();
      }
    },
  );
});
