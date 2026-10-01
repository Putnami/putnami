import { describe, expect } from 'bun:test';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { ClientGenerationError, readOpenApiSource } from '../../src/generator/openapi-reader';
import type { SpecIR } from '../../src/generator/ir.type';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const WORKSPACE_ROOT = resolve(import.meta.dir, '..', '..', '..', '..', '..');
const CLIENT_ROOT = resolve(import.meta.dir, '..', '..');
const FIXTURE = join(WORKSPACE_ROOT, 'protocols/clientcontract/fixtures/openapi/valid/provider-websocket.openapi.json');

function providerWireSpec(): SpecIR {
  return readOpenApiSource(readFileSync(FIXTURE, 'utf8'), { mode: 'firstParty' });
}

describe('provider-owned websocket wires in the TypeScript emitter', () => {
  specTest(
    'emits a byte stream and a frame stream from the shared corpus and typechecks them',
    {
      feature: 'typescript/service-clients',
      requirement: 'provider-owned-websocket-wires',
      check: 'the-emitted-typescript-client-returns-a-byte-stream-or-a-frame-stream',
    },
    () => {
      const files = generateTypeScriptClient(providerWireSpec(), { packageName: '@test/gateway-client' });
      const services = files
        .filter((file) => file.path.startsWith('src/') && file.path !== 'src/types.ts' && file.path !== 'src/index.ts')
        .map((file) => file.content)
        .join('\n');
      expect(services).toContain(
        'async connectDatabase(input: ConnectDatabaseInput, options?: ClientCallOptions): Promise<ByteStream> {',
      );
      expect(services).toContain('return this.serviceByteStream(\'GET\', "/v1/databases/connect", {');
      expect(services).toContain(
        'subscribeEvents(options?: ClientCallOptions): FrameStream<SubscribeEventsSend, SubscribeEventsMessage> {',
      );
      expect(services).toContain('return this.serviceFrameStream<SubscribeEventsSend, SubscribeEventsMessage>(');
      // No frame vocabulary reaches the emitted code: the runtime owns the
      // socket, the provider owns the frames.
      expect(services).not.toContain('putnami.service.v1');
      expect(services).not.toContain('serviceBidiStream');
      const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
      expect(types).toContain('export type SubscribeEventsSend = EventClientFrame;');
      expect(types).not.toContain('ConnectDatabaseMessage');

      const root = mkdtempSync(join(tmpdir(), 'clientgen-provider-wire-'));
      try {
        for (const file of files) {
          const destination = join(root, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content);
        }
        writeFileSync(
          join(root, 'tsconfig.json'),
          JSON.stringify({
            compilerOptions: {
              noEmit: true,
              strict: true,
              noImplicitOverride: true,
              noPropertyAccessFromIndexSignature: true,
              noImplicitReturns: true,
              module: 'esnext',
              moduleResolution: 'bundler',
              target: 'esnext',
              lib: ['ESNext', 'DOM'],
              types: [],
              skipLibCheck: true,
              paths: { '@putnami/client': [join(CLIENT_ROOT, 'src/index.ts')] },
            },
            include: ['src/**/*.ts'],
          }),
        );
        const tsc = join(WORKSPACE_ROOT, 'node_modules', '.bin', 'tsc');
        expect(existsSync(tsc), 'workspace TypeScript compiler').toBe(true);
        const result = spawnSync(tsc, ['--project', join(root, 'tsconfig.json')], { cwd: root, encoding: 'utf8' });
        expect(`${result.stdout ?? ''}${result.stderr ?? ''}`).toBe('');
        expect(result.status).toBe(0);
      } finally {
        rmSync(root, { force: true, recursive: true });
      }
    },
    120_000,
  );

  specTest(
    'refuses a provider-owned wire whose shape the emitter cannot carry',
    {
      feature: 'typescript/service-clients',
      requirement: 'provider-owned-websocket-wires',
      check: 'the-emitted-typescript-client-returns-a-byte-stream-or-a-frame-stream',
    },
    () => {
      const spec = providerWireSpec();
      for (const service of spec.services) {
        for (const method of service.methods) {
          if (method.operationId === 'subscribeEvents' && method.client) {
            (method.client as { messages?: unknown }).messages = undefined;
          }
        }
      }
      expect(() => generateTypeScriptClient(spec, { packageName: '@test/gateway-client' })).toThrow(/subscribeEvents/);
      // The reader names the same refusal the Go reader names for the same bytes.
      const source = readFileSync(FIXTURE, 'utf8').replace(
        '"transports": [\n            {\n              "protocol": "websocket",\n              "path": "/events/ws"',
        '"transports": [\n            { "protocol": "sse", "path": "/events/sse", "encoding": "json" },\n            {\n              "protocol": "websocket",\n              "path": "/events/ws"',
      );
      let thrown: unknown;
      try {
        readOpenApiSource(source, { mode: 'firstParty' });
      } catch (error) {
        thrown = error;
      }
      expect(thrown).toBeInstanceOf(ClientGenerationError);
      expect((thrown as ClientGenerationError).contractCode).toBe('client_contract.invalid_transport');
    },
  );
});
