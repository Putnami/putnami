import { getDirectoryName, toPosixPath } from '@putnami/utils';
import { convertBracketToParam } from '../shared/route-tree.utils';

export const asRoute = (fileName: string) => {
  // A route key uses forward slashes whatever separator the scan yielded.
  let route = getDirectoryName(toPosixPath(fileName));
  route = route
    .split('/')
    .filter((s) => s && s.charAt(0) !== '(' && s.length > 0)
    .map(convertBracketToParam)
    .join('/');
  const lastChar = route.charAt(route.length - 1);
  if (lastChar === '.' || lastChar === '/') {
    route = route.substring(0, route.length - 1);
  }
  if (route.charAt(0) !== '/') {
    route = `/${route}`;
  }
  return route;
};

export const normalizeRoutePrefix = (prefix?: string): string | undefined => {
  if (!prefix) return undefined;
  const trimmed = prefix.trim();
  if (!trimmed) return undefined;
  const withLeadingSlash = trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
  const normalized = withLeadingSlash.replace(/\/+/g, '/');
  if (normalized === '/') return '/';
  return normalized.endsWith('/') ? normalized.slice(0, -1) : normalized;
};

export const applyRoutePrefix = (route: string, prefix?: string): string => {
  const normalizedPrefix = normalizeRoutePrefix(prefix);
  if (!normalizedPrefix || normalizedPrefix === '/') return route;
  if (route === '/') return normalizedPrefix;
  return `${normalizedPrefix}${route}`;
};

export const inModuleOrDefault = <T, M extends object = Record<string, unknown>>(
  module: M | undefined,
  ...elemNames: (keyof M)[]
): T | undefined => {
  if (!module) {
    return undefined;
  }
  for (const elemName of elemNames) {
    if (module[elemName]) {
      return module[elemName] as T;
    }
  }
  // Dynamic modules may have a 'default' export not captured in the generic M type
  return (module as Record<string, unknown>)['default'] as T;
};
