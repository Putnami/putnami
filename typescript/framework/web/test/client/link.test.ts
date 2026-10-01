import { describe, expect, it } from 'bun:test';
import { Link, type LinkProps } from '../../src/client/components';
import type { PrefetchBehavior } from '../../src/client/hooks';

/**
 * Tests for Link component with prefetching support.
 *
 * Note: Testing actual rendering and prefetching behavior requires a DOM environment
 * and React Router context, which is complex to set up in Bun test environment currently.
 * These tests verify exports and type signatures.
 */
describe('Link Component', () => {
  describe('Link export', () => {
    it('is exported as a component', () => {
      expect(Link).toBeDefined();
      expect(typeof Link).toBe('object'); // React.forwardRef returns an object
    });

    it('has displayName', () => {
      expect(Link.displayName).toBe('Link');
    });
  });

  describe('LinkProps type', () => {
    it('extends RouterLinkProps', () => {
      // Type assertion test - verifies LinkProps extends RouterLinkProps
      const _props: LinkProps = {
        to: '/test',
        prefetch: 'none',
      };
      expect(_props.to).toBe('/test');
      expect(_props.prefetch).toBe('none');
    });

    it('accepts prefetch prop with PrefetchBehavior type', () => {
      // Type assertion test - verifies prefetch prop accepts PrefetchBehavior
      const _props1: LinkProps = {
        to: '/test',
        prefetch: 'intent' as PrefetchBehavior,
      };
      const _props2: LinkProps = {
        to: '/test',
        prefetch: 'render' as PrefetchBehavior,
      };
      const _props3: LinkProps = {
        to: '/test',
        prefetch: 'none' as PrefetchBehavior,
      };
      const _props4: LinkProps = {
        to: '/test',
        // prefetch is optional
      };

      expect(_props1.prefetch).toBe('intent');
      expect(_props2.prefetch).toBe('render');
      expect(_props3.prefetch).toBe('none');
      expect(_props4.prefetch).toBeUndefined();
    });

    it('accepts all RouterLinkProps', () => {
      // Type assertion test - verifies all standard Link props work
      const _props: LinkProps = {
        to: '/dashboard',
        prefetch: 'intent',
        replace: false,
        state: { from: '/home' },
        reloadDocument: false,
      };
      expect(_props.to).toBe('/dashboard');
      expect(_props.prefetch).toBe('intent');
    });
  });
});
