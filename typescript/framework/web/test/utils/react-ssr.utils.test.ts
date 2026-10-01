import { describe, expect, it } from 'bun:test';
import { applyRoutePrefix, asRoute, inModuleOrDefault, normalizeRoutePrefix } from '../../src/ssr/react-ssr.utils';

describe('react-ssr.utils', () => {
  describe('asRoute', () => {
    it('converts a file path to a route path', () => {
      expect(asRoute('pages/home/page.tsx')).toBe('/pages/home');
    });

    it('filters out route groups (parentheses)', () => {
      expect(asRoute('(auth)/login/page.tsx')).toBe('/login');
    });

    it('handles root paths', () => {
      expect(asRoute('./page.tsx')).toBe('/');
    });

    it('handles nested paths with route groups', () => {
      expect(asRoute('(marketing)/products/(featured)/item/page.tsx')).toBe('/products/item');
    });

    it('strips trailing slashes and dots', () => {
      expect(asRoute('folder./page.tsx')).toBe('/folder');
    });

    it('ensures route starts with slash', () => {
      expect(asRoute('api/users/page.tsx')).toBe('/api/users');
    });

    it('derives the same route from a Windows-separated scan path', () => {
      expect(asRoute('blog\\[slug]\\page.tsx')).toBe('/blog/:slug');
      expect(asRoute('(marketing)\\products\\page.tsx')).toBe('/products');
    });

    it('converts dynamic segments to React Router params', () => {
      expect(asRoute('blog/[slug]/page.tsx')).toBe('/blog/:slug');
    });

    it('converts catch-all segments to wildcard', () => {
      expect(asRoute('[...path]/page.tsx')).toBe('/*');
    });

    it('converts optional catch-all to wildcard', () => {
      expect(asRoute('[[...path]]/page.tsx')).toBe('/*');
    });

    it('handles mixed dynamic and static segments', () => {
      expect(asRoute('users/[id]/posts/[postId]/page.tsx')).toBe('/users/:id/posts/:postId');
    });
  });

  describe('route prefix helpers', () => {
    it('normalizes route prefixes', () => {
      expect(normalizeRoutePrefix('projects')).toBe('/projects');
      expect(normalizeRoutePrefix('/projects/')).toBe('/projects');
      expect(normalizeRoutePrefix('/')).toBe('/');
      expect(normalizeRoutePrefix('')).toBeUndefined();
      expect(normalizeRoutePrefix(undefined)).toBeUndefined();
    });

    it('applies prefixes to routes', () => {
      expect(applyRoutePrefix('/items', '/projects')).toBe('/projects/items');
      expect(applyRoutePrefix('/', '/projects')).toBe('/projects');
      expect(applyRoutePrefix('/items', '/')).toBe('/items');
      expect(applyRoutePrefix('/items')).toBe('/items');
    });
  });

  describe('inModuleOrDefault', () => {
    it('returns undefined for undefined module', () => {
      expect(inModuleOrDefault(undefined, 'loader')).toBeUndefined();
    });

    it('returns undefined for null module', () => {
      expect(inModuleOrDefault(null, 'loader')).toBeUndefined();
    });

    it('returns the named export if found', () => {
      const module = { loader: () => 'data', other: () => 'other' };
      expect(inModuleOrDefault(module, 'loader')).toBe(module.loader);
    });

    it('returns the first matching named export', () => {
      const module = { action: () => 'action', loader: () => 'loader' };
      expect(inModuleOrDefault(module, 'loader', 'action')).toBe(module.loader);
    });

    it('returns default export if no named export matches', () => {
      const defaultFn = () => 'default';
      const module = { default: defaultFn };
      expect(inModuleOrDefault(module, 'loader')).toBe(defaultFn);
    });

    it('returns undefined if no match and no default', () => {
      const module = { other: () => 'other' };
      expect(inModuleOrDefault(module, 'loader')).toBeUndefined();
    });
  });
});
