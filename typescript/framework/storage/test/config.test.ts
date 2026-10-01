import { describe, expect, it } from 'bun:test';
import { StorageConfig, processTokenSecret, resolveTokenSecret } from '../src/config';

describe('StorageConfig', () => {
  it('should export the config definition', () => {
    expect(StorageConfig).toBeDefined();
  });
});

describe('resolveTokenSecret', () => {
  it('should return the configured secret when one is provided', () => {
    expect(resolveTokenSecret('explicit-secret')).toBe('explicit-secret');
  });

  it('should fall back to the stable per-process secret when unset', () => {
    // The fallback must be identical across calls within a process so URLs
    // minted by storage() validate in storageServer(); it is intentionally NOT
    // stable across processes (callers must configure tokenSecret for that).
    expect(resolveTokenSecret()).toBe(processTokenSecret);
    expect(resolveTokenSecret(undefined)).toBe(processTokenSecret);
    expect(resolveTokenSecret()).toBe(resolveTokenSecret(undefined));
  });

  it('should treat an empty string as unset and fall back', () => {
    expect(resolveTokenSecret('')).toBe(processTokenSecret);
  });
});
