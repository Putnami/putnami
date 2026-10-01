import { afterEach, describe, expect, it } from 'bun:test';
import {
  getPrimaryDatasource,
  normalizeDatasource,
  primaryDatasourceName,
  primarySearchPath,
  resolveDatasourceName,
  setPrimaryDatasource,
} from '../src/primary-datasource';

afterEach(() => {
  // Reset the process-global so cases don't bleed into each other (or other files).
  setPrimaryDatasource(undefined);
});

describe('normalizeDatasource', () => {
  it('returns undefined when no datasource is declared', () => {
    expect(normalizeDatasource(undefined)).toBeUndefined();
  });

  it('accepts the bare-name shorthand', () => {
    expect(normalizeDatasource('identity')).toEqual({ name: 'identity' });
  });

  it('trims a bare name and treats a blank one as no primary', () => {
    expect(normalizeDatasource('  identity  ')).toEqual({ name: 'identity' });
    expect(normalizeDatasource('   ')).toBeUndefined();
    expect(normalizeDatasource('')).toBeUndefined();
  });

  it('accepts the { name, schema } object form', () => {
    expect(normalizeDatasource({ name: 'identity', schema: 'identity_auth' })).toEqual({
      name: 'identity',
      schema: 'identity_auth',
    });
  });

  it('trims object fields and drops a blank schema', () => {
    expect(normalizeDatasource({ name: '  identity  ', schema: '  identity_auth  ' })).toEqual({
      name: 'identity',
      schema: 'identity_auth',
    });
    expect(normalizeDatasource({ name: 'identity', schema: '   ' })).toEqual({ name: 'identity' });
  });

  it('treats a blank object name as no primary (schema ignored without a name)', () => {
    expect(normalizeDatasource({ name: '', schema: 'orphan' })).toBeUndefined();
  });
});

describe('primary datasource state', () => {
  it('round-trips through set/get and clears with undefined', () => {
    expect(getPrimaryDatasource()).toBeUndefined();
    expect(primaryDatasourceName()).toBeUndefined();

    setPrimaryDatasource({ name: 'identity', schema: 'identity_auth' });
    expect(getPrimaryDatasource()).toEqual({ name: 'identity', schema: 'identity_auth' });
    expect(primaryDatasourceName()).toBe('identity');

    setPrimaryDatasource(undefined);
    expect(getPrimaryDatasource()).toBeUndefined();
    expect(primaryDatasourceName()).toBeUndefined();
  });
});

describe('resolveDatasourceName', () => {
  it('returns the explicit datasource as-is, ignoring the primary', () => {
    setPrimaryDatasource({ name: 'identity' });
    expect(resolveDatasourceName('wealth')).toBe('wealth');
  });

  it('inherits the primary when no explicit datasource is given', () => {
    setPrimaryDatasource({ name: 'identity' });
    expect(resolveDatasourceName(undefined)).toBe('identity');
    // An empty explicit name is treated as "unset" and also inherits the primary.
    expect(resolveDatasourceName('')).toBe('identity');
  });

  it('stays undefined (=> default downstream) when neither is set', () => {
    expect(resolveDatasourceName(undefined)).toBeUndefined();
  });
});

describe('primarySearchPath', () => {
  it('is undefined when no primary is declared', () => {
    expect(primarySearchPath('identity')).toBeUndefined();
  });

  it('is undefined when the primary declares no schema', () => {
    setPrimaryDatasource({ name: 'identity' });
    expect(primarySearchPath('identity')).toBeUndefined();
  });

  it('returns the schema only for the primary datasource name', () => {
    setPrimaryDatasource({ name: 'identity', schema: 'identity_auth' });
    expect(primarySearchPath('identity')).toBe('identity_auth');
    // A different datasource gets its schema from a binding/server default, not the plugin.
    expect(primarySearchPath('wealth')).toBeUndefined();
    expect(primarySearchPath('default')).toBeUndefined();
  });
});
