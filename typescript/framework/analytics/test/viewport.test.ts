import { describe, expect, it } from 'bun:test';
import { clientViewportClass } from '../src/client/viewport';
import { viewportClassOf } from '../src/server/enrich/viewport';
import { VIEWPORT_CLASSES } from '../src/server/sanitize/vocabulary';

// Ten widths: the bound of every bucket and the value just below it, so a
// changed comparison operator shows up as a bucket change.
const WIDTHS = [320, 575, 576, 767, 768, 991, 992, 1199, 1200, 2560];
const EXPECTED = ['xs', 'xs', 'sm', 'sm', 'md', 'md', 'lg', 'lg', 'xl', 'xl'];

describe('viewportClassOf', () => {
  it('buckets the five documented widths', () => {
    expect(WIDTHS.map(viewportClassOf)).toEqual(EXPECTED);
  });

  it('only ever emits a protocol viewport class', () => {
    for (const width of WIDTHS) {
      expect(VIEWPORT_CLASSES).toContain(viewportClassOf(width));
    }
  });

  // The tracker cannot import a server file, so the bounds exist twice. This is
  // the test that keeps the copy from drifting.
  it('agrees with the browser copy on every width', () => {
    for (const width of WIDTHS) {
      expect(clientViewportClass(width)).toBe(viewportClassOf(width));
    }
  });
});
