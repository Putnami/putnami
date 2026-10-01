import { getDirectoryName, toPosixPath } from '@putnami/utils';

export const asRoute = (fileName: string) => {
  // A route key uses forward slashes whatever separator the scan yielded.
  let route = getDirectoryName(toPosixPath(fileName));
  const lastChar = route.charAt(route.length - 1);
  if (lastChar === '.' || lastChar === '/') {
    route = route.substring(0, route.length - 1);
  }

  return route.length === 0 ? '/' : route;
};

/**
 * Apply a prefix to a route path.
 * Handles edge cases: root path `/`, empty path, trailing slashes.
 *
 * @example
 * applyPrefix('/', '/tasks')       → '/tasks'
 * applyPrefix('/[id]', '/tasks')   → '/tasks/[id]'
 * applyPrefix('[id]', '/tasks')    → '/tasks/[id]'
 * applyPrefix('/', undefined)      → '/'
 */
export function applyPrefix(path: string, prefix?: string): string {
  if (!prefix) {
    return path.startsWith('/') ? path : `/${path}`;
  }
  const p = prefix.endsWith('/') ? prefix.slice(0, -1) : prefix;
  if (path === '/' || path === '') return p || '/';
  const normalized = path.startsWith('/') ? path : `/${path}`;
  return `${p}${normalized}`;
}
