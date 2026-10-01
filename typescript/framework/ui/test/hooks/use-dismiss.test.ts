import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type ContainsLike, isDismissKey, isOutsideClick } from '../../src/hooks/use-dismiss';

/**
 * These tests exercise the pure helpers that back the overlay dismissal effects in
 * Dropdown and Popover. The full behaviour — attaching the document `mousedown`/`keydown`
 * listeners while open and calling `close()` — runs only in the browser and needs a real
 * DOM: the project's bun test env renders with `renderToStaticMarkup` (no effects, no
 * events) and a DOM test environment cannot be added without a new dependency. The
 * helpers below encode the dismissal decisions and are fully unit-testable against stub
 * nodes, which is where the bugs (e.g. a flipped containment check) would live.
 */

/** Builds a stub node whose `contains` returns true only for the listed descendants. */
const stubNode = (...descendants: unknown[]): ContainsLike => ({
  contains: (other: unknown) => descendants.includes(other),
});

describe('isOutsideClick', () => {
  specTest(
    'returns true when the target is outside both the surface and the trigger',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'an-outside-click-dismisses-the-surface',
    },
    () => {
      const target = { id: 'elsewhere' };
      expect(isOutsideClick(target, stubNode(), stubNode())).toBe(true);
    },
  );

  it('returns false when the target is inside the surface', () => {
    const target = { id: 'inside-menu' };
    expect(isOutsideClick(target, stubNode(target), stubNode())).toBe(false);
  });

  specTest(
    'returns false when the target is inside the trigger',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'a-click-on-the-triggering-node-does-not-dismiss',
    },
    () => {
      const target = { id: 'on-trigger' };
      expect(isOutsideClick(target, stubNode(), stubNode(target))).toBe(false);
    },
  );

  it('treats the surface/trigger node itself as contained (not an outside click)', () => {
    const surface = stubNode();
    // A node always contains itself in the DOM; mirror that the click on the surface
    // node is not "outside".
    expect(isOutsideClick(surface, stubNode(surface), stubNode())).toBe(false);
  });

  specTest(
    'does not dismiss while a ref is still unattached (null surface or trigger)',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'an-unattached-ref-never-dismisses',
    },
    () => {
      const target = { id: 'elsewhere' };
      expect(isOutsideClick(target, null, stubNode())).toBe(false);
      expect(isOutsideClick(target, stubNode(), null)).toBe(false);
      expect(isOutsideClick(target, null, null)).toBe(false);
    },
  );
});

describe('isDismissKey', () => {
  specTest(
    'treats Escape as a dismiss key',
    { feature: 'typescript/ui-system', requirement: 'keyboard-and-dismissal', check: 'escape-is-a-dismiss-key' },
    () => {
      expect(isDismissKey('Escape')).toBe(true);
    },
  );

  it('ignores other keys', () => {
    expect(isDismissKey('Enter')).toBe(false);
    expect(isDismissKey('Esc')).toBe(false);
    expect(isDismissKey('a')).toBe(false);
    expect(isDismissKey('')).toBe(false);
  });
});
