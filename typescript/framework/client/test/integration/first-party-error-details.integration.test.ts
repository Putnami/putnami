import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, http, Int, openapi } from '@putnami/application';
import { ArrayOf, ConflictException } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import { ClientFrameworkError } from '../../src/runtime/errors';

/**
 * A TypeScript provider declares an error's `details` body with
 * `.mayThrowDetails()`; the generated TypeScript client, emitted from the
 * document the provider publishes and loaded from disk, calls it on a real
 * port and exposes the details typed on the error for that code. The Go half
 * is go/framework/openapi/error_details_test.go.
 */

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

const sampleRejection = {
  rejections: [{ index: 0, project: 'a', error: 'image not found' }],
  gcp_response_body: '{"error":{"code":409}}',
  retryable: false,
};

describe('declared error details: TypeScript provider to generated TypeScript consumer', () => {
  specTest(
    'the generated client exposes the declared details on the typed error for that code',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-error-safety',
      check: 'a-provider-declared-details-body-arrives-typed-at-the-generated-client',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'deploys', audience: 'api://deploys' }, credentials: {} },
      });
      providerApi.register(
        '/deploys',
        endpoint()
          .body({ projects: ArrayOf(String) })
          .returns({ id: String })
          .mayThrowDetails('Conflict', {
            rejections: ArrayOf({ index: Int, project: String, error: String }),
            gcp_response_body: String,
            retryable: Boolean,
          })
          .mayThrow('NotFound')
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'non-idempotent' } })
          .handle(() => {
            throw new ConflictException(sampleRejection);
          }),
        'POST',
      );
      const providerOpenApi = openapi({ title: 'Deploys', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      await provider.start();
      try {
        const server = providerHttp.getServer();
        if (!server) throw new Error('provider HTTP server did not start');
        const document = providerOpenApi.spec();
        if (!document) throw new Error('provider OpenAPI contract was not emitted');
        const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
        const method = ir.services.flatMap((service) => service.methods).find((entry) => entry.path === '/deploys');
        if (!method) throw new Error('the provider operation did not reach the generated IR');

        const directory = mkdtempSync(join(tmpdir(), 'putnami-error-details-client-'));
        temporaryDirectories.push(directory);
        for (const file of generateTypeScriptClient(ir, { packageName: '@test/deploys-client' })) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
        const DeploysClient = generated['DeploysClient'] as new (config: {
          baseUrl: string;
          transport: 'http';
        }) => Record<string, (...args: never[]) => Promise<unknown>>;
        const client = new DeploysClient({ baseUrl: `http://localhost:${server.port}`, transport: 'http' });
        const deploy = client[method.name] as (input: { body: { projects: string[] } }) => Promise<unknown>;

        const failure = await deploy.call(client, { body: { projects: ['a'] } }).catch((error: unknown) => error);
        expect(failure).toBeInstanceOf(ClientFrameworkError);
        expect(failure).toMatchObject({
          service: 'deploys',
          method: method.operationId,
          status: 409,
          code: 'conflict',
        });
        // Decoded, not copied: a declared `Int` is 64 bits wide, so it arrives as a bigint.
        expect((failure as ClientFrameworkError).details).toEqual({
          ...sampleRejection,
          rejections: [{ index: 0n, project: 'a', error: 'image not found' }],
        });

        const guardName = (code: string) =>
          Object.keys(generated).find((name) => name.startsWith('is') && name.endsWith(`${code}Error`)) ?? '';
        const isConflict = generated[guardName('Conflict')] as (error: unknown) => boolean;
        const isNotFound = generated[guardName('NotFound')] as (error: unknown) => boolean;
        expect(isConflict(failure)).toBe(true);
        expect(isNotFound(failure)).toBe(false);
      } finally {
        await provider.stop();
      }
    },
    30_000,
  );
});
