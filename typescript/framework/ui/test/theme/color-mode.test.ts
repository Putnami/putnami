import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  buildColorModeScript,
  isColorMode,
  isColorModeScript,
  parseStoredColorMode,
  resolveColorMode,
  STORAGE_KEY,
} from '../../src/theme/color-mode';

/**
 * Exercises the pure color-mode helpers that back ThemeProvider's persistence and FOUC
 * handling. The provider itself wires these into effects that only run in the browser;
 * the project's bun test env renders with `renderToStaticMarkup` (no effects), so these
 * decisions are tested here against plain values rather than a live DOM.
 */

describe('isColorMode', () => {
  it('accepts the three known modes', () => {
    expect(isColorMode('light')).toBe(true);
    expect(isColorMode('dark')).toBe(true);
    expect(isColorMode('system')).toBe(true);
  });

  it('rejects anything else', () => {
    expect(isColorMode('Light')).toBe(false);
    expect(isColorMode('')).toBe(false);
    expect(isColorMode(null)).toBe(false);
    expect(isColorMode(undefined)).toBe(false);
    expect(isColorMode(0)).toBe(false);
  });
});

describe('parseStoredColorMode', () => {
  it('returns the stored mode when valid', () => {
    expect(parseStoredColorMode('light')).toBe('light');
    expect(parseStoredColorMode('dark')).toBe('dark');
    expect(parseStoredColorMode('system')).toBe('system');
  });

  it('returns null for a missing or stale value', () => {
    expect(parseStoredColorMode(null)).toBeNull();
    expect(parseStoredColorMode('')).toBeNull();
    expect(parseStoredColorMode('auto')).toBeNull();
  });
});

describe('resolveColorMode', () => {
  it('returns an explicit mode unchanged regardless of the system setting', () => {
    expect(resolveColorMode('light', 'dark')).toBe('light');
    expect(resolveColorMode('dark', 'light')).toBe('dark');
  });

  it('follows the system setting when the preference is "system"', () => {
    expect(resolveColorMode('system', 'dark')).toBe('dark');
    expect(resolveColorMode('system', 'light')).toBe('light');
  });
});

describe('buildColorModeScript', () => {
  const script = buildColorModeScript();

  specTest(
    'reads the persisted preference from the storage key',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'the-script-reads-the-persisted-preference-before-paint',
    },
    () => {
      expect(script).toContain(STORAGE_KEY);
      expect(script).toContain('localStorage.getItem(s)');
    },
  );

  specTest(
    'falls back to the OS preference when the mode is "system"',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'the-script-falls-back-to-the-os-preference-for-system',
    },
    () => {
      expect(script).toContain("m==='system'");
      expect(script).toContain('prefers-color-scheme:dark');
    },
  );

  specTest(
    'applies the resolved mode to <html> as both attribute and color-scheme',
    {
      feature: 'typescript/ui-system',
      requirement: 'mode-reactive-colors',
      check: 'the-resolved-mode-is-applied-to-html-as-attribute-and-color-scheme',
    },
    () => {
      expect(script).toContain('document.documentElement.dataset.colorMode=m;');
      expect(script).toContain('document.documentElement.style.colorScheme=m;');
    },
  );

  specTest(
    'is a self-invoking, storage-failure-tolerant IIFE',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'the-bootstrap-script-is-a-storage-failure-tolerant-iife',
    },
    () => {
      expect(script.startsWith('(function(){')).toBe(true);
      expect(script.endsWith('})()')).toBe(true);
      expect(script).toContain('catch(e)');
    },
  );

  it('honours a custom storage key, escaping it as a JS string literal', () => {
    const custom = buildColorModeScript("a'key");
    expect(custom).toContain('var s="a\'key";');
  });
});

describe('isColorModeScript', () => {
  specTest(
    'matches a previously injected color-mode script',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'hydration-recognizes-a-previously-injected-script',
    },
    () => {
      expect(isColorModeScript({ children: buildColorModeScript() })).toBe(true);
    },
  );

  specTest(
    'ignores unrelated or non-string script children',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'an-unrelated-script-child-is-not-mistaken-for-the-bootstrap',
    },
    () => {
      expect(isColorModeScript({ children: 'console.log("hi")' })).toBe(false);
      expect(isColorModeScript({ children: undefined })).toBe(false);
      expect(isColorModeScript({})).toBe(false);
    },
  );
});
