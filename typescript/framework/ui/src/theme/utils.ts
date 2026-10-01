import type { Theme } from './theme';

function isObject(value: unknown): value is object {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return isObject(value);
}

/** Recursively merges a partial theme into a base theme. */
export function deepMerge<T extends object>(target: T, source: Partial<T>): T {
  const result = { ...target } as T;
  for (const key in source) {
    const sourceValue = source[key as keyof T];
    const targetValue = target[key as keyof T];
    if (isObject(sourceValue) && isObject(targetValue)) {
      (result as Record<string, unknown>)[key] = deepMerge(targetValue as object, sourceValue as object);
    } else if (sourceValue !== undefined) {
      (result as Record<string, unknown>)[key] = sourceValue;
    }
  }
  return result;
}

/** Retrieves a value from a nested object using dot-notation path. */
export const getThemeValue = (obj: unknown, path: string | number): unknown => {
  if (!obj || path == null) return undefined;
  const pathStr = String(path);
  if (!pathStr.includes('.')) {
    return isRecord(obj) ? obj[pathStr] : undefined;
  }

  return pathStr.split('.').reduce<unknown>((acc, part) => (isRecord(acc) ? acc[part] : undefined), obj);
};

/** Resolves a color string against the theme's color tokens. */
export const getColor = (theme: Theme, value: string): string => {
  const themeColor = getThemeValue(theme.colors, value);
  if (typeof themeColor === 'string') return themeColor;
  return value;
};

/** Resolves a spacing value (number or token name) against the theme. */
export const getSpacing = (theme: Theme, value: string | number): string | number => {
  if (typeof value === 'number') {
    return theme.spacing(value);
  }
  // Look up token from theme.space (xs, sm, md, lg, xl, xxl)
  const spaceValue = theme.space[value as keyof typeof theme.space];
  if (spaceValue) {
    return spaceValue;
  }
  return value;
};
