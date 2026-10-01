import { afterEach, describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { NotFoundException } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, application, endpoint, http, Uuid, type ClientServiceContract } from '@putnami/application';
import { openapi } from '../../src/openapi/openapi.plugin';
import { createTestApp } from '../../src/testing';
import type { TestApp } from '../../src/testing/test-app';

/**
 * Cross-language conformance: the merged `protocols/clientcontract/fixtures` corpus and
 * `protocols/clientcontract/schemas/x-putnami-client-v1.json` are authoritative for D0.1/D0.2.
 * These tests build a real provider (`api()` + `endpoint()`), emit its OpenAPI document through
 * the real `openapi()` plugin, and check the result against corpus fixture values so the TS
 * provider's own generation-time validator (`validateClientServiceContract`/`validateClientOperation`
 * in `../../src/openapi/openapi.ts`) is exercised against real corpus data, mirroring what
 * `protocols/clientcontract/conformance_test.go` proves for the Go reader.
 *
 * The checks these tests observe belong to THIS project's `client-contract-conformance`
 * requirement, not to the contract's `strict-provider-authority`. A check can only be
 * observed by a suite that runs, and no dependency edge pulls a TypeScript project into
 * the impacted set of a Go module: a requirement in `protocols/clientcontract` naming a
 * check only this suite writes is `missing` on every run that does not happen to select
 * `@putnami/application`. The two requirements are linked by the `dependsOn` relation
 * `typescript/api-contracts` declares on `client-contract/first-party-generated-clients`.
 */

const FIXTURE_ROOT = join(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/openapi');

function readFixture(relativePath: string): Record<string, unknown> {
  return JSON.parse(readFileSync(join(FIXTURE_ROOT, relativePath), 'utf8'));
}

describe('client contract corpus conformance', () => {
  specTest(
    'accepts the full fixture service contract and getWidget operation policy losslessly',
    {
      feature: 'typescript/api-contracts',
      requirement: 'client-contract-conformance',
      check: 'the-corpus-service-contract-and-operation-policy-project-losslessly',
    },
    async () => {
      const fixture = readFixture('valid/full.openapi.json') as {
        'x-putnami-client': {
          service: { id: string; audience: string };
          credentials: Record<string, unknown>;
          defaults?: { resilience?: Record<string, unknown> };
        };
        paths: Record<string, Record<string, { 'x-putnami-client': Record<string, unknown> }>>;
      };
      const { service, credentials, defaults } = fixture['x-putnami-client'];
      // Document-level contract taken verbatim from the corpus (protobuf/protocolVersion are
      // framework-derived, not provider input — see `ClientServiceContract`).
      const client = { service, credentials, defaults } as ClientServiceContract;

      const getWidget = fixture.paths['/widgets/{id}'].get['x-putnami-client'] as {
        security: { alternatives: readonly { allOf: readonly { profile: string }[] }[] };
        idempotency: { kind: 'safe' | 'idempotent' | 'non-idempotent' };
        resilience?: Record<string, unknown>;
      };

      const apiPlugin = api({ autoScan: false, client });
      // `.secure()` is intentionally not called: the corpus's second alternative (`caller`,
      // forwarded-user-token) carries no scopes, so a `.secure({ scopes })` guard would reject
      // it (see `validateSecurityIsNotWeaker`). `.client()` security stays declarative-only here,
      // which is the exact shape the corpus fixture itself describes.
      apiPlugin.register(
        '/widgets/[id]',
        endpoint()
          .params({ id: Uuid })
          .returns({ id: Uuid })
          .client({
            security: getWidget.security as never,
            idempotency: getWidget.idempotency,
            resilience: getWidget.resilience,
          })
          .handle(() => ({ id: '00000000-0000-0000-0000-000000000000' })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'Corpus fixture', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);

      await app.start();
      try {
        const spec = openapiPlugin.spec();
        expect(spec?.['x-putnami-client']).toEqual({
          protocolVersion: 1,
          service,
          credentials,
          ...(defaults ? { defaults } : {}),
        });

        const operation = spec?.paths['/widgets/{id}']?.get['x-putnami-client'];
        expect(operation.security).toEqual(getWidget.security);
        expect(operation.idempotency).toEqual(getWidget.idempotency);
        expect(operation.resilience).toEqual(getWidget.resilience);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'rejects an idempotency key header that collides with a framework-owned header',
    {
      feature: 'typescript/api-contracts',
      requirement: 'client-contract-conformance',
      check: 'an-unrepresentable-corpus-semantic-is-refused-at-registration',
    },
    async () => {
      const fixture = readFixture('invalid/idempotency-identity-header-collision.openapi.json') as {
        paths: Record<string, Record<string, { 'x-putnami-client': { idempotency: { keyHeader: string } } }>>;
      };
      const keyHeader = fixture.paths['/items'].post['x-putnami-client'].idempotency.keyHeader;

      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'invalid', audience: 'urn:invalid' }, credentials: {} },
      });
      expect(() =>
        apiPlugin.register(
          '/items',
          endpoint()
            .body({ name: String })
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'idempotent', keyHeader },
            })
            .handle(() => ({})),
          'POST',
        ),
      ).not.toThrow(); // registration is lazy; validation happens at spec generation time

      const openapiPlugin = openapi({ title: 'Corpus fixture (invalid)', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);
      await expect(app.start()).rejects.toThrow('idempotency.keyHeader is framework-owned');
    },
  );

  describe('L02 — client contract behavior', () => {
    it('declares a 201 success response for a first-party client operation', async () => {
      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'widgets', audience: 'api://widgets' }, credentials: {} },
      });
      apiPlugin.register(
        '/widgets',
        endpoint()
          .body({ name: String })
          .response(201, 'Created', { id: Uuid, name: String })
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'non-idempotent' } })
          .handle(() => ({ id: '00000000-0000-0000-0000-000000000000', name: 'widget' })),
        'POST',
      );
      const openapiPlugin = openapi({ title: 'L02', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);
      await app.start();
      try {
        const responses = openapiPlugin.spec()?.paths['/widgets'].post.responses;
        expect(responses['201'].description).toBe('Created');
        expect(responses['201'].content?.['application/json'].schema).toEqual({
          type: 'object',
          properties: { id: { type: 'string', format: 'uuid' }, name: { type: 'string' } },
          required: ['id', 'name'],
          additionalProperties: false,
        });
      } finally {
        await app.stop();
      }
    });

    it('supports AND-within-alternative and OR-across-alternatives client security', async () => {
      const apiPlugin = api({
        autoScan: false,
        client: {
          service: { id: 'widgets', audience: 'api://widgets' },
          credentials: {
            service: { kind: 'service-token' },
            tenant: { kind: 'named-header', header: 'X-Tenant-ID' },
            caller: { kind: 'forwarded-user-token' },
          },
        },
      });
      apiPlugin.register(
        '/widgets/[id]',
        endpoint()
          .params({ id: Uuid })
          .returns({ id: Uuid })
          .client({
            security: {
              alternatives: [
                { allOf: [{ profile: 'service' }, { profile: 'tenant' }] },
                { allOf: [{ profile: 'caller' }] },
              ],
            },
            idempotency: { kind: 'safe' },
          })
          .handle(() => ({ id: '00000000-0000-0000-0000-000000000000' })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'L02', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);
      await app.start();
      try {
        const security = openapiPlugin.spec()?.paths['/widgets/{id}'].get['x-putnami-client'].security;
        expect(security.alternatives).toHaveLength(2);
        expect(security.alternatives[0].allOf).toHaveLength(2); // AND
        expect(security.alternatives[1].allOf).toHaveLength(1); // OR branch
      } finally {
        await app.stop();
      }
    });

    it('keeps declared errors independently scoped per operation for the same error code', async () => {
      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'widgets', audience: 'api://widgets' }, credentials: {} },
      });
      apiPlugin.register(
        '/widgets/[id]',
        endpoint()
          .params({ id: Uuid })
          .returns({ id: Uuid })
          .mayThrowWith('Conflict', { retryable: false })
          .throws(409, 'Widget locked', { reason: String })
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
          .handle(() => ({ id: '00000000-0000-0000-0000-000000000000' })),
        'GET',
      );
      apiPlugin.register(
        '/orders/[id]',
        endpoint()
          .params({ id: Uuid })
          .returns({ id: Uuid })
          .mayThrowWith('Conflict', { retryable: true })
          .throws(409, 'Order already shipped')
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
          .handle(() => ({ id: '00000000-0000-0000-0000-000000000000' })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'L02', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);
      await app.start();
      try {
        const spec = openapiPlugin.spec();
        // D0.1: `.mayThrowWith('Conflict', ...)` is the PascalCase DX affordance; the wire
        // code projected into `x-putnami-client` is the stable, dotted `errorCodeToStableCode`
        // value ('conflict'), mirroring go/framework/errors's `CodeConflict`.
        const widgetError = spec?.paths['/widgets/{id}'].get['x-putnami-client'].errors.find(
          (error: { code: string }) => error.code === 'conflict',
        );
        const orderError = spec?.paths['/orders/{id}'].get['x-putnami-client'].errors.find(
          (error: { code: string }) => error.code === 'conflict',
        );
        expect(widgetError).toEqual({
          status: 409,
          code: 'conflict',
          retryable: false,
          schema: expect.objectContaining({ properties: { reason: { type: 'string' } } }),
        });
        expect(orderError).toEqual({ status: 409, code: 'conflict', retryable: true });
      } finally {
        await app.stop();
      }
    });

    it('does not leave a removed route or its schema behind in a fresh generation', async () => {
      const buildSpec = async (includeSecondRoute: boolean) => {
        const apiPlugin = api({
          autoScan: false,
          client: { service: { id: 'widgets', audience: 'api://widgets' }, credentials: {} },
        });
        apiPlugin.register(
          '/widgets/[id]',
          endpoint()
            .params({ id: Uuid })
            .returns({ id: Uuid })
            .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
            .handle(() => ({ id: '00000000-0000-0000-0000-000000000000' })),
          'GET',
        );
        if (includeSecondRoute) {
          apiPlugin.register(
            '/widgets/archived',
            endpoint()
              .returns({ archivedCount: Number })
              .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
              .handle(() => ({ archivedCount: 0 })),
            'GET',
          );
        }
        const openapiPlugin = openapi({ title: 'L02', version: '1.0.0' });
        const app = application()
          .use(http({ port: 0 }))
          .use(apiPlugin)
          .use(openapiPlugin);
        await app.start();
        try {
          return openapiPlugin.spec();
        } finally {
          await app.stop();
        }
      };

      const withBoth = await buildSpec(true);
      expect(withBoth?.paths['/widgets/archived']).toBeDefined();

      const afterDeletion = await buildSpec(false);
      expect(afterDeletion?.paths['/widgets/archived']).toBeUndefined();
      expect(Object.keys(afterDeletion?.paths ?? {})).toEqual(['/widgets/{id}']);
    });

    it('rejects a first-party client operation with an unsupported schema constraint', () => {
      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'widgets', audience: 'api://widgets' }, credentials: {} },
      });
      const custom = {
        __schema: 'putnami:schema',
        baseType: 'string',
        constraints: [{ name: 'custom', validate: () => true, message: 'custom' }],
        // biome-ignore lint/suspicious/noExplicitAny: constructing a deliberately unsupported schema descriptor
      } as any;
      apiPlugin.register(
        '/widgets',
        endpoint()
          .body({ value: custom })
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'non-idempotent' } })
          .handle(() => ({})),
        'POST',
      );
      const openapiPlugin = openapi({ title: 'L02', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(openapiPlugin);
      expect(app.start()).rejects.toThrow('unsupported validation constraint');
    });
  });

  describe('D0.1 — first-party error envelope', () => {
    let testApp: TestApp | undefined;

    afterEach(async () => {
      await testApp?.stop();
      testApp = undefined;
    });

    specTest(
      'serializes an unhandled framework exception as the stable {code,error,message} envelope',
      {
        feature: 'typescript/api-contracts',
        requirement: 'client-contract-conformance',
        check: 'an-unhandled-framework-exception-serializes-as-the-stable-envelope',
      },
      async () => {
        const apiPlugin = api({
          autoScan: false,
          client: { service: { id: 'widgets', audience: 'api://widgets' }, credentials: {} },
        });
        apiPlugin.register(
          '/widgets/[id]',
          endpoint()
            .params({ id: Uuid })
            .returns({ id: Uuid })
            .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
            .handle(() => {
              throw new NotFoundException('nope');
            }),
          'GET',
        );
        testApp = await createTestApp({ plugins: [apiPlugin] });

        const res = await testApp.fetch('/widgets/00000000-0000-0000-0000-000000000000');
        expect(res.status).toBe(404);
        expect(await res.json()).toEqual({ code: 'not_found', error: 'Not Found', message: 'nope' });
      },
    );

    it('leaves the default {statusCode,message,error} body untouched for a non-first-party route', async () => {
      const apiPlugin = api({ autoScan: false });
      apiPlugin.register(
        '/widgets/[id]',
        endpoint()
          .params({ id: Uuid })
          .returns({ id: Uuid })
          .handle(() => {
            throw new NotFoundException('nope');
          }),
        'GET',
      );
      testApp = await createTestApp({ plugins: [apiPlugin] });

      const res = await testApp.fetch('/widgets/00000000-0000-0000-0000-000000000000');
      expect(res.status).toBe(404);
      expect(await res.json()).toEqual({ statusCode: 404, message: 'nope', error: 'Not Found' });
    });
  });
});
