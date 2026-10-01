import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { api, application, endpoint, http, type Module, module, openapi } from '@putnami/application';
import { ClientGeneratorPlugin } from '../../src/generator/client-generator.plugin';

// The plugin never touches its `owner` argument (spec reading is disk-based), so
// a bare object stands in for the Module here.
const fakeModule = {} as Module;
const DEFAULT_BIOME_CONFIG = JSON.stringify({
  ...JSON.parse(readFileSync(join(import.meta.dir, '../../../../../biome.json'), 'utf8')),
  vcs: { enabled: false },
});
const FORMATTER_TEST_TIMEOUT_MS = 60_000;

function writeJson(path: string, value: unknown): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(value));
}

const openapiDoc = {
  openapi: '3.0.3',
  info: { title: 'Widgets', version: '1.2.3' },
  paths: {
    '/widgets': {
      get: {
        operationId: 'listWidgets',
        responses: {
          '200': {
            description: 'OK',
            content: {
              'application/json': {
                schema: { type: 'object', properties: { id: { type: 'string' } }, required: ['id'] },
              },
            },
          },
        },
      },
    },
  },
};

/** Three operations under one first path segment, so they land on one client. */
const multiFeatureDoc = {
  openapi: '3.0.3',
  info: { title: 'Platform', version: '1.0.0' },
  paths: {
    '/v1/operator/cli-usage': { get: { operationId: 'getV1_Operator_Cli-usage', responses: {} } },
    '/v1/billing/invoices': { get: { operationId: 'getV1_Billing_Invoices', responses: {} } },
    '/v1/health': { get: { operationId: 'getV1_Health', responses: {} } },
  },
};

describe('ClientGeneratorPlugin.postGenerate', () => {
  let projectRoot: string;
  let originalProjectRoot: string | undefined;

  beforeEach(() => {
    originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    projectRoot = mkdtempSync(join(tmpdir(), 'putnami-clientgen-'));
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
    writeFileSync(join(projectRoot, 'package.json'), JSON.stringify({ name: '@demo/widgets', version: '1.2.3' }));
    writeFileSync(join(projectRoot, 'biome.json'), DEFAULT_BIOME_CONFIG);
  });

  afterEach(() => {
    if (originalProjectRoot === undefined) {
      delete process.env.PUTNAMI_PROJECT_ROOT;
    } else {
      process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
    }
    rmSync(projectRoot, { recursive: true, force: true });
  });

  test('always emits .gen/clientgen/config.json with the resolved schema', async () => {
    const result = await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule);

    const configPath = join(projectRoot, '.gen/clientgen/config.json');
    expect(existsSync(configPath)).toBe(true);
    expect(result.assets?.['.gen/clientgen/config.json']).toBe(configPath);
    expect(JSON.parse(readFileSync(configPath, 'utf8'))).toEqual({
      thirdParty: true,
      targets: ['ts'],
      ts: { output: 'clients/ts', packageName: '@demo/widgets-client' },
      go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
    });
  });

  test('allows an explicit third-party configuration with no generated spec', async () => {
    const result = await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule);

    expect(existsSync(join(projectRoot, '.gen/clientgen/config.json'))).toBe(true);
    expect(existsSync(join(projectRoot, 'clients/ts/src/index.ts'))).toBe(false);
    expect(Object.keys(result.assets ?? {})).toEqual(['.gen/clientgen/config.json']);
  });

  test('fails first-party generation when the provider emitted no client contract', async () => {
    expect(new ClientGeneratorPlugin().postGenerate(fakeModule)).rejects.toMatchObject({
      code: 'clientgen_first_party_required',
    });
  });

  // A module that declares a first-party contract for a subset of a workload's
  // routes registers them on a manual api(). Its openapi() used to read only a
  // scanned folder, so the build published no contract and this generator
  // failed the pre-build hook for a contract the provider had declared.
  test(
    'generates a first-party client from the routes a manual api() module registered',
    async () => {
      const internal = module('auth-server-internal')
        .use(
          api({
            autoScan: false,
            csrf: true,
            client: {
              service: { id: 'auth-server', audience: 'auth-server' },
              credentials: { service: { kind: 'service-token' } },
            },
          }).register(
            '/internal/_repro',
            endpoint()
              .csrfExempt()
              .body({ reference: String })
              .response(204, 'Revoked')
              .client({
                security: { alternatives: [{ allOf: [{ profile: 'service' }] }] },
                idempotency: { kind: 'idempotent' },
              })
              .handle(() => undefined),
            'POST',
          ),
        )
        .use(openapi({ title: 'x', version: '1.0.0' }))
        .use(new ClientGeneratorPlugin({ targets: ['ts'] }));

      await application()
        .use(http({ port: 0 }))
        .use(internal)
        .build({ publishCapabilityManifest: false, publishDesignGraph: false });

      const clientFile = join(projectRoot, 'clients/ts/src/internal-client.ts');
      expect(existsSync(clientFile)).toBe(true);
      expect(readFileSync(clientFile, 'utf8')).toContain("path: '/internal/_repro'");
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  // An application with a public document beside the contract module has two
  // openapi() documents. The contract takes schema/openapi.json whatever the
  // registration order, so the generator reads the contract, not the public one.
  test(
    'generates the client from the contract document when a public openapi() is registered first',
    async () => {
      const internal = module('auth-server-internal')
        .use(
          api({
            autoScan: false,
            client: {
              service: { id: 'auth-server', audience: 'auth-server' },
              credentials: { service: { kind: 'service-token' } },
            },
          }).register(
            '/internal/_repro',
            endpoint()
              .csrfExempt()
              .body({ reference: String })
              .response(204, 'Revoked')
              .client({
                security: { alternatives: [{ allOf: [{ profile: 'service' }] }] },
                idempotency: { kind: 'idempotent' },
              })
              .handle(() => undefined),
            'POST',
          ),
        )
        .use(openapi({ title: 'internal', version: '1.0.0' }))
        .use(new ClientGeneratorPlugin({ targets: ['ts'] }));

      const result = await application()
        .use(http({ port: 0 }))
        .use(
          api({ autoScan: false }).register(
            '/public',
            endpoint(() => ({ ok: true })),
            'GET',
          ),
        )
        .use(openapi({ title: 'public', version: '1.0.0' }))
        .use(internal)
        .build({ publishCapabilityManifest: false, publishDesignGraph: false });

      expect(JSON.parse(readFileSync(join(projectRoot, 'schema/openapi.json'), 'utf8')).info.title).toBe('internal');
      expect(result.assets?.['schema/openapi-1.json']).toEndWith('/.gen/schema/openapi-1.json');
      const clientFile = join(projectRoot, 'clients/ts/src/internal-client.ts');
      expect(readFileSync(clientFile, 'utf8')).toContain("path: '/internal/_repro'");
      expect(readFileSync(clientFile, 'utf8')).not.toContain('/public');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test('never routes first-party Proto generation through the lossy legacy reader', async () => {
    writeJson(join(projectRoot, '.gen/schema/api.proto.json'), {
      packageName: 'demo.v1',
      content: 'syntax = "proto3";',
      serviceMeta: {},
      messageMeta: {},
      enumTypes: [],
    });

    expect(new ClientGeneratorPlugin().postGenerate(fakeModule)).rejects.toMatchObject({
      code: 'clientgen_first_party_required',
    });
  });

  test(
    'bootstraps a missing TS client before consumer typecheck from the fully-written contract',
    async () => {
      writeJson(join(projectRoot, '.gen/schema/openapi.json'), openapiDoc);

      const result = await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule);

      const clientFile = join(projectRoot, 'clients/ts/src/widgets-client.ts');
      expect(existsSync(clientFile)).toBe(true);
      expect(readFileSync(clientFile, 'utf8')).toContain('export class WidgetsClient');
      expect(result.assets?.['client/ts/src/widgets-client.ts']).toBe(clientFile);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'generates the TS client from the generated OpenAPI asset path',
    async () => {
      const openapiPath = join(projectRoot, 'schema/openapi.json');
      writeJson(openapiPath, openapiDoc);

      await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule, {
        assets: { 'schema/openapi.json': openapiPath },
      });

      expect(existsSync(join(projectRoot, 'clients/ts/src/widgets-client.ts'))).toBe(true);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'generates the TS client from a custom OpenAPI asset path',
    async () => {
      const openapiPath = join(projectRoot, '.gen/custom-openapi.json');
      writeJson(openapiPath, openapiDoc);

      await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule, {
        assets: { 'schema/openapi.json': openapiPath },
      });

      expect(existsSync(join(projectRoot, 'clients/ts/src/widgets-client.ts'))).toBe(true);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'prefers OpenAPI over a Proto artifact when both exist',
    async () => {
      const openapiPath = join(projectRoot, 'schema/openapi.json');
      writeJson(openapiPath, openapiDoc);
      writeJson(join(projectRoot, '.gen/schema/api.proto.json'), {
        packageName: 'demo.v1',
        content: 'syntax = "proto3";',
        serviceMeta: {
          ThingService: [
            {
              name: 'GetThing',
              requestMessage: 'Req',
              responseMessage: 'Res',
              clientStreaming: false,
              serverStreaming: false,
            },
          ],
        },
        messageMeta: { Req: [], Res: [] },
        enumTypes: [],
      });

      await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(fakeModule, {
        assets: { 'schema/openapi.json': openapiPath },
      });

      expect(existsSync(join(projectRoot, 'clients/ts/src/widgets-client.ts'))).toBe(true);
      expect(existsSync(join(projectRoot, 'clients/ts/src/thing-client.ts'))).toBe(false);
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  test(
    'refreshes a tracked client with a provider method added in the same build',
    async () => {
      const specPath = join(projectRoot, '.gen/schema/openapi.json');
      writeJson(specPath, openapiDoc);
      const plugin = new ClientGeneratorPlugin({ thirdParty: true });
      await plugin.postGenerate(fakeModule);
      const clientPath = join(projectRoot, 'clients/ts/src/widgets-client.ts');
      expect(readFileSync(clientPath, 'utf8')).not.toContain('async createWidgets(');

      writeJson(specPath, {
        ...openapiDoc,
        paths: {
          ...openapiDoc.paths,
          '/widgets/new': {
            post: {
              operationId: 'createWidgets',
              responses: { '204': { description: 'Created' } },
            },
          },
        },
      });

      await plugin.postGenerate(fakeModule);
      expect(readFileSync(clientPath, 'utf8')).toContain('async createWidgets(');
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );

  // The regression fixture: one generated client whose operations come from two
  // feature modules plus one module that declares none. Ownership is the module
  // that owns the ApiPlugin the endpoint was registered on, so the unattributed
  // operation must never borrow the client generator's own feature.
  test(
    'attributes each operation to the module that owns its endpoint',
    async () => {
      writeJson(join(projectRoot, '.gen/schema/openapi.json'), multiFeatureDoc);

      const operatorApi = api({ autoScan: false });
      operatorApi.register(
        '/v1/operator/cli-usage',
        endpoint(() => ({ total: 0 })),
        'GET',
      );
      const billingApi = api({ autoScan: false });
      billingApi.register(
        '/v1/billing/invoices',
        endpoint(() => ({ total: 0 })),
        'GET',
      );
      const coreApi = api({ autoScan: false });
      coreApi.register(
        '/v1/health',
        endpoint(() => ({ total: 0 })),
        'GET',
      );

      const app = application()
        .use(
          module('operator')
            .feature({
              id: 'platform/operator-cli-usage',
              name: 'Operator CLI usage',
              outcome: 'Operators can inspect CLI usage',
              owner: 'platform',
            })
            .use(operatorApi),
        )
        .use(
          module('billing')
            .feature({ id: 'platform/billing', name: 'Billing', outcome: 'Customers are billed', owner: 'platform' })
            .use(billingApi),
        )
        .use(module('core').use(coreApi));

      await new ClientGeneratorPlugin({ thirdParty: true }).postGenerate(app);

      const config = JSON.parse(readFileSync(join(projectRoot, '.gen/clientgen/config.json'), 'utf8'));
      expect(config.design.operations).toEqual([
        {
          method: 'GET',
          path: '/v1/billing/invoices',
          producerProject: expect.any(String),
          producerFeature: 'platform/billing',
        },
        {
          method: 'GET',
          path: '/v1/operator/cli-usage',
          producerProject: expect.any(String),
          producerFeature: 'platform/operator-cli-usage',
        },
      ]);

      const client = readFileSync(join(projectRoot, 'clients/ts/src/v1client.ts'), 'utf8');
      expect(client).toContain("operationId: 'getV1_Operator_Cli-usage'");
      expect(client).toContain("producerFeature: 'platform/operator-cli-usage'");
      expect(client).toContain("producerFeature: 'platform/billing'");
      expect(client).toContain("operationId: 'getV1_Health'");
      expect(client).toContain('async getV1_Operator_Cli_usage(');
      expect(client).toContain("operationId: 'getV1_Operator_Cli-usage',");
    },
    FORMATTER_TEST_TIMEOUT_MS,
  );
});
