import { describe, expect, it } from 'bun:test';
import { hasRegisteredModules, loadRegisteredModule, registerModuleLoader } from '../../src/bundled/module-registry';

describe('module-registry', () => {
  it('registerModuleLoader and loadRegisteredModule', async () => {
    const testModule = { default: 'test-value', name: 'test' };
    registerModuleLoader('test-module', async () => testModule);

    const loaded = await loadRegisteredModule('test-module');
    expect(loaded).toBe(testModule);
  });

  it('loadRegisteredModule returns undefined for unregistered key', async () => {
    const loaded = await loadRegisteredModule('nonexistent-module');
    expect(loaded).toBeUndefined();
  });

  it('hasRegisteredModules returns true when modules are registered', () => {
    registerModuleLoader('another-module', async () => ({}));
    expect(hasRegisteredModules()).toBe(true);
  });

  it('registerModuleLoader overwrites existing loader', async () => {
    registerModuleLoader('overwrite-key', async () => ({ version: 1 }));
    registerModuleLoader('overwrite-key', async () => ({ version: 2 }));

    const loaded = await loadRegisteredModule('overwrite-key');
    expect(loaded).toEqual({ version: 2 });
  });
});
