import type { ViewportClass } from '../sanitize/vocabulary';

export type { ViewportClass } from '../sanitize/vocabulary';

/**
 * Buckets a viewport width into the five closed classes of the protocol
 * (body §D.7).
 *
 * The width itself is never stored: it is a fingerprinting surface, and five
 * buckets answer every question a layout decision needs. The browser runs the
 * same five bounds from `src/client/viewport.ts`, which cannot import this
 * file; `test/viewport.test.ts` asserts the two copies agree.
 *
 * @param width - The viewport width in CSS pixels.
 * @returns The bucket the width falls in.
 */
export function viewportClassOf(width: number): ViewportClass {
  if (width < 576) {
    return 'xs';
  }
  if (width < 768) {
    return 'sm';
  }
  if (width < 992) {
    return 'md';
  }
  if (width < 1200) {
    return 'lg';
  }
  return 'xl';
}
