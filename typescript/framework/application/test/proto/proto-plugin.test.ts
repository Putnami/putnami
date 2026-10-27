import { rmSync } from 'node:fs';
import { afterAll, describe, expect, it } from 'bun:test';
import { application, api, http } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { fileExists, getProjectRoot, joinPath } from '@putnami/utils';
import { computeProtoPackageName, proto } from '../../src/proto/proto.plugin';

const FIXTURES_API_PATH = `${import.meta.dir}/../openapi/fixtures/api`;

describe('computeProtoPackageName', () => {
  specTest(
    'should convert scoped package to dot-separated name',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'the-default-package-name-derives-from-the-npm-package-name',
    },
    () => {
      expect(computeProtoPackageName('@putnami/application')).toBe('putnami.application.v1');
    },
  );

  it('should convert dashes to dots', () => {
    expect(computeProtoPackageName('my-cool-api')).toBe('my.cool.api.v1');
  });

  it('should convert underscores to dots', () => {
    expect(computeProtoPackageName('my_app')).toBe('my.app.v1');
  });

  it('should handle scoped packages with dashes', () => {
    expect(computeProtoPackageName('@my-org/my-api')).toBe('my.org.my.api.v1');
  });

  it('should collapse multiple separators', () => {
    expect(computeProtoPackageName('@org/my--app')).toBe('org.my.app.v1');
  });

  it('should lowercase the result', () => {
    expect(computeProtoPackageName('MyApp')).toBe('myapp.v1');
  });

  specTest(
    'should return api.v1 for undefined input',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'a-missing-package-name-falls-back-to-api-v1',
    },
    () => {
      expect(computeProtoPackageName(undefined)).toBe('api.v1');
    },
  );

  specTest(
    'should return api.v1 for empty string',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'a-missing-package-name-falls-back-to-api-v1',
    },
    () => {
      expect(computeProtoPackageName('')).toBe('api.v1');
    },
  );

  it('should handle simple package name', () => {
    expect(computeProtoPackageName('server')).toBe('server.v1');
  });

  it('should handle package name with only special chars', () => {
    expect(computeProtoPackageName('@/')).toBe('api.v1');
  });
});

describe('ProtoPlugin output path', () => {
  const projectRoot = getProjectRoot();
  const committedPath = joinPath(projectRoot, 'schema', 'api.proto');
  const fallbackPath = joinPath(projectRoot, '.gen', 'schema', 'api.proto');

  afterAll(() => {
    try {
      rmSync(committedPath, { force: true });
      rmSync(joinPath(projectRoot, 'schema'), { recursive: true, force: true });
      rmSync(fallbackPath, { force: true });
    } catch {}
  });

  specTest(
    'should write api.proto to <project>/schema/api.proto by default',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'the-proto-is-written-under-schema-by-default',
    },
    async () => {
      try {
        rmSync(committedPath, { force: true });
      } catch {}

      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const protoPlugin = proto({ packageName: 'committed.v1' });
      const app = application().use(httpPlugin).use(apiPlugin).use(protoPlugin);

      try {
        const result = await app.build();
        expect(result.assets?.['schema/api.proto']).toBe(committedPath);
        expect(fileExists(committedPath)).toBe(true);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'should write to .gen/schema/api.proto when output is false',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'output-false-writes-the-proto-under-gen',
    },
    async () => {
      try {
        rmSync(fallbackPath, { force: true });
      } catch {}

      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const protoPlugin = proto({ packageName: 'fallback.v1', output: false });
      const app = application().use(httpPlugin).use(apiPlugin).use(protoPlugin);

      try {
        const result = await app.build();
        expect(result.assets?.['schema/api.proto']).toBe(fallbackPath);
        expect(fileExists(fallbackPath)).toBe(true);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'should honour a custom output path',
    {
      feature: 'typescript/grpc-services',
      requirement: 'proto-output',
      check: 'a-custom-output-path-is-honoured',
    },
    async () => {
      const customPath = joinPath(projectRoot, '.gen', 'custom-api.proto');
      try {
        rmSync(customPath, { force: true });
      } catch {}

      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const protoPlugin = proto({ packageName: 'custom.v1', output: '.gen/custom-api.proto' });
      const app = application().use(httpPlugin).use(apiPlugin).use(protoPlugin);

      try {
        const result = await app.build();
        expect(result.assets?.['schema/api.proto']).toBe(customPath);
        expect(fileExists(customPath)).toBe(true);
      } finally {
        await app.stop();
      }
    },
  );
});
