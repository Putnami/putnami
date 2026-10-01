import { describe, expect, it } from 'bun:test';
import { DatasourceSchemaConflictError, resolveDatasourceSchemas } from '../src';

describe('resolveDatasourceSchemas', () => {
  it('maps each datasource to the schema its sources declare', () => {
    const schemas = resolveDatasourceSchemas([
      { datasource: 'marketing', schema: 'marketing', namespace: 'app' },
      { datasource: 'marketing', schema: 'marketing', namespace: 'putnami-analytics' },
      { datasource: 'identity', schema: 'identity_auth', namespace: 'iam' },
    ]);

    expect([...schemas]).toEqual([
      ['marketing', 'marketing'],
      ['identity', 'identity_auth'],
    ]);
  });

  it('lets a source without a schema follow the one another source declares', () => {
    const schemas = resolveDatasourceSchemas([
      { datasource: 'marketing', namespace: 'app' },
      { datasource: 'marketing', schema: '', namespace: 'legacy' },
      { datasource: 'marketing', schema: 'marketing', namespace: 'putnami-analytics' },
      { datasource: 'default', namespace: 'orphan' },
    ]);

    expect([...schemas]).toEqual([['marketing', 'marketing']]);
  });

  it('rejects one datasource in two schemas, naming both sources', () => {
    const resolve = () =>
      resolveDatasourceSchemas([
        { datasource: 'marketing', schema: 'marketing', namespace: 'app' },
        { datasource: 'marketing', schema: 'public', namespace: 'putnami-analytics' },
      ]);

    expect(resolve).toThrow(DatasourceSchemaConflictError);
    expect(resolve).toThrow(
      'datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")',
    );
  });
});
