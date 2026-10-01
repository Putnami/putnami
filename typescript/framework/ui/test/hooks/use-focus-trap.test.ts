import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { computeTrapTarget, filterFocusable, isFocusable } from '../../src/hooks/use-focus-trap';

/**
 * These tests exercise the pure helpers that back useFocusTrap. The full
 * focus-movement behaviour (storing/restoring document.activeElement, moving focus
 * on open, and Tab/Shift+Tab cycling) runs only in the browser and needs a real DOM;
 * the project's bun test env exposes only a minimal `document` stub (no element tree,
 * no activeElement, no .focus()), and a DOM test environment cannot be added without
 * a new dependency. The helpers below encode the trap's decision logic and are fully
 * unit-testable against fake element lists, which is where the bugs would live.
 */

/** Builds a stub element with the attribute surface isFocusable/filterFocusable read. */
const stubEl = (attrs: Record<string, string> = {}) => ({
  getAttribute: (name: string): string | null => attrs[name] ?? null,
  hasAttribute: (name: string): boolean => name in attrs,
});

describe('useFocusTrap helpers', () => {
  describe('isFocusable', () => {
    it('treats a plain element as focusable', () => {
      expect(isFocusable(stubEl())).toBe(true);
    });

    specTest(
      'rejects disabled elements',
      { feature: 'typescript/ui-system', requirement: 'focus-boundary', check: 'a-disabled-node-is-not-focusable' },
      () => {
        expect(isFocusable(stubEl({ disabled: '' }))).toBe(false);
      },
    );

    specTest(
      'rejects aria-hidden elements',
      { feature: 'typescript/ui-system', requirement: 'focus-boundary', check: 'an-aria-hidden-node-is-not-focusable' },
      () => {
        expect(isFocusable(stubEl({ 'aria-hidden': 'true' }))).toBe(false);
      },
    );

    specTest(
      'rejects elements removed from the tab order (negative tabindex)',
      {
        feature: 'typescript/ui-system',
        requirement: 'focus-boundary',
        check: 'a-negative-tabindex-node-is-not-focusable',
      },
      () => {
        expect(isFocusable(stubEl({ tabindex: '-1' }))).toBe(false);
      },
    );

    it('keeps elements with an explicit non-negative tabindex', () => {
      expect(isFocusable(stubEl({ tabindex: '0' }))).toBe(true);
      expect(isFocusable(stubEl({ tabindex: '3' }))).toBe(true);
    });
  });

  describe('filterFocusable', () => {
    it('drops non-tabbable candidates while preserving order', () => {
      const first = stubEl();
      const disabled = stubEl({ disabled: '' });
      const hidden = stubEl({ 'aria-hidden': 'true' });
      const last = stubEl({ tabindex: '0' });

      expect(filterFocusable([first, disabled, hidden, last])).toEqual([first, last]);
    });
  });

  describe('computeTrapTarget', () => {
    const [a, b, c] = ['a', 'b', 'c'];
    const list = [a, b, c];

    it('returns null when there is nothing focusable', () => {
      expect(computeTrapTarget([], null, false)).toBeNull();
    });

    specTest(
      'uses the fallback target when there are no focusable descendants',
      {
        feature: 'typescript/ui-system',
        requirement: 'focus-boundary',
        check: 'the-surface-is-the-fallback-when-no-descendant-is-focusable',
      },
      () => {
        expect(computeTrapTarget([], null, false, 'container')).toBe('container');
        expect(computeTrapTarget([], 'container', true, 'container')).toBe('container');
      },
    );

    specTest(
      'wraps from the last element to the first on Tab',
      {
        feature: 'typescript/ui-system',
        requirement: 'focus-boundary',
        check: 'focus-wraps-from-the-last-element-to-the-first',
      },
      () => {
        expect(computeTrapTarget(list, c, false)).toBe(a);
      },
    );

    specTest(
      'wraps from the first element to the last on Shift+Tab',
      {
        feature: 'typescript/ui-system',
        requirement: 'focus-boundary',
        check: 'focus-wraps-from-the-first-element-to-the-last',
      },
      () => {
        expect(computeTrapTarget(list, a, true)).toBe(c);
      },
    );

    it('lets the browser handle mid-list Tab (no forced target)', () => {
      expect(computeTrapTarget(list, b, false)).toBeNull();
      expect(computeTrapTarget(list, b, true)).toBeNull();
    });

    it('does not force a target tabbing off the first or last in the natural direction', () => {
      // Tab from the first → browser moves to the second on its own.
      expect(computeTrapTarget(list, a, false)).toBeNull();
      // Shift+Tab from the last → browser moves to the second-last on its own.
      expect(computeTrapTarget(list, c, true)).toBeNull();
    });

    specTest(
      'pulls focus back inside when it has escaped the container',
      { feature: 'typescript/ui-system', requirement: 'focus-boundary', check: 'escaped-focus-is-pulled-back-inside' },
      () => {
        expect(computeTrapTarget(list, null, false)).toBe(a);
        expect(computeTrapTarget(list, null, true)).toBe(c);
        expect(computeTrapTarget(list, 'outsider', false)).toBe(a);
      },
    );

    it('handles a single focusable element by trapping on itself', () => {
      const only = [a];
      expect(computeTrapTarget(only, a, false)).toBe(a);
      expect(computeTrapTarget(only, a, true)).toBe(a);
    });
  });
});
