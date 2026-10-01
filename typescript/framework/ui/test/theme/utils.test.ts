import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { deepMerge, getColor, getSpacing, getThemeValue } from '../../src/theme/utils';
import { defaultTheme, darkTheme } from '../../src/theme/default';

describe('deepMerge', () => {
  it('merges two flat objects', () => {
    const target = { a: 1, b: 2 };
    const source = { b: 3, c: 4 };
    expect(deepMerge(target, source)).toEqual({ a: 1, b: 3, c: 4 });
  });

  it('deeply merges nested objects', () => {
    const target = { colors: { primary: 'blue', secondary: 'gray' } };
    const source = { colors: { primary: 'red' } };
    expect(deepMerge(target, source)).toEqual({ colors: { primary: 'red', secondary: 'gray' } });
  });

  it('does not mutate the target', () => {
    const target = { a: 1, nested: { x: 10 } };
    const source = { nested: { x: 99 } };
    deepMerge(target, source);
    expect(target.nested.x).toBe(10);
  });

  it('does not mutate the source', () => {
    const target = { a: 1 };
    const source = { b: 2 };
    deepMerge(target, source);
    expect(source).toEqual({ b: 2 });
  });

  it('skips undefined values from source', () => {
    const target = { a: 1, b: 2 };
    const source = { a: undefined };
    const result = deepMerge(target, source as Partial<typeof target>);
    expect(result.a).toBe(1);
  });

  it('overwrites scalar with scalar', () => {
    const target = { count: 5 };
    const source = { count: 10 };
    expect(deepMerge(target, source)).toEqual({ count: 10 });
  });

  it('overwrites object with non-object (source wins)', () => {
    const target = { nested: { x: 1 } };
    const source = { nested: 'flat' as unknown as { x: number } };
    const result = deepMerge(target, source);
    expect(result.nested).toBe('flat');
  });

  it('handles three levels of nesting', () => {
    const target = { a: { b: { c: 1, d: 2 } } };
    const source = { a: { b: { c: 99 } } };
    expect(deepMerge(target, source)).toEqual({ a: { b: { c: 99, d: 2 } } });
  });

  it('returns a new object, not the target reference', () => {
    const target = { a: 1 };
    const result = deepMerge(target, {});
    expect(result).not.toBe(target);
  });

  it('handles empty source', () => {
    const target = { a: 1, b: { c: 2 } };
    expect(deepMerge(target, {})).toEqual(target);
  });

  it('handles arrays as scalars (does not deep-merge arrays)', () => {
    const target = { items: [1, 2, 3] };
    const source = { items: [4, 5] };
    expect(deepMerge(target, source)).toEqual({ items: [4, 5] });
  });
});

describe('getThemeValue', () => {
  const obj = {
    colors: {
      primary: {
        main: '#14b8a6',
        500: '#14b8a6',
      },
      white: '#ffffff',
    },
    spacing: 4,
  };

  it('retrieves a top-level value', () => {
    expect(getThemeValue(obj, 'spacing')).toBe(4);
  });

  it('retrieves a nested value via dot-path', () => {
    expect(getThemeValue(obj, 'colors.white')).toBe('#ffffff');
  });

  it('retrieves a deeply nested value', () => {
    expect(getThemeValue(obj, 'colors.primary.main')).toBe('#14b8a6');
  });

  it('returns undefined for a missing path', () => {
    expect(getThemeValue(obj, 'colors.missing')).toBeUndefined();
  });

  it('returns undefined when path is deeply missing', () => {
    expect(getThemeValue(obj, 'a.b.c.d')).toBeUndefined();
  });

  it('returns undefined when obj is null', () => {
    expect(getThemeValue(null, 'colors')).toBeUndefined();
  });

  it('returns undefined when obj is undefined', () => {
    expect(getThemeValue(undefined, 'colors')).toBeUndefined();
  });

  it('converts numeric path to string for lookup', () => {
    expect(getThemeValue(obj, 'colors.primary.500')).toBe('#14b8a6');
  });

  it('handles numeric path argument', () => {
    const arr = { 0: 'zero', 1: 'one' };
    expect(getThemeValue(arr, 0)).toBe('zero');
  });
});

describe('getColor', () => {
  specTest(
    'returns the theme color for a valid dot-path token',
    {
      feature: 'typescript/ui-system',
      requirement: 'mode-reactive-colors',
      check: 'a-semantic-color-token-resolves-through-the-theme',
    },
    () => {
      expect(getColor(defaultTheme, 'white')).toBe('#ffffff');
    },
  );

  it('returns nested semantic color', () => {
    expect(getColor(defaultTheme, 'primary.main')).toBe('#14b8a6');
  });

  it('returns the raw value when the token is not found in the theme', () => {
    expect(getColor(defaultTheme, '#custom-hex')).toBe('#custom-hex');
  });

  it('returns the raw value when the resolved theme value is not a string', () => {
    // 'primary' resolves to an object (SemanticColor), not a string → passthrough
    const result = getColor(defaultTheme, 'primary');
    expect(result).toBe('primary');
  });

  specTest(
    'resolves dark theme colors correctly',
    {
      feature: 'typescript/ui-system',
      requirement: 'mode-reactive-colors',
      check: 'the-dark-palette-resolves-its-own-colors',
    },
    () => {
      expect(getColor(darkTheme, 'primary.main')).toBe('#2dd4bf');
    },
  );

  it('returns css color strings unchanged', () => {
    expect(getColor(defaultTheme, 'rgb(255,0,0)')).toBe('rgb(255,0,0)');
  });
});

describe('getSpacing', () => {
  it('calls theme.spacing for numeric values', () => {
    const result = getSpacing(defaultTheme, 4);
    expect(result).toBe('1rem'); // 4 * 0.25rem
  });

  it('returns 0rem for factor 0', () => {
    expect(getSpacing(defaultTheme, 0)).toBe('0rem');
  });

  it('returns correct spacing for factor 1', () => {
    expect(getSpacing(defaultTheme, 1)).toBe('0.25rem');
  });

  it('returns correct spacing for factor 8', () => {
    expect(getSpacing(defaultTheme, 8)).toBe('2rem');
  });

  it('resolves space token "xs"', () => {
    expect(getSpacing(defaultTheme, 'xs')).toBe('0.25rem');
  });

  it('resolves space token "sm"', () => {
    expect(getSpacing(defaultTheme, 'sm')).toBe('0.5rem');
  });

  it('resolves space token "md"', () => {
    expect(getSpacing(defaultTheme, 'md')).toBe('1rem');
  });

  it('resolves space token "lg"', () => {
    expect(getSpacing(defaultTheme, 'lg')).toBe('1.5rem');
  });

  it('resolves space token "xl"', () => {
    expect(getSpacing(defaultTheme, 'xl')).toBe('2rem');
  });

  it('resolves space token "xxl"', () => {
    expect(getSpacing(defaultTheme, 'xxl')).toBe('3rem');
  });

  it('passes through unknown string values', () => {
    expect(getSpacing(defaultTheme, 'auto')).toBe('auto');
  });

  it('passes through CSS values like "100%"', () => {
    expect(getSpacing(defaultTheme, '100%')).toBe('100%');
  });
});
