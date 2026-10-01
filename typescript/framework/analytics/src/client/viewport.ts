/** The five viewport buckets of the protocol, copied for the browser bundle. */
type ClientViewportClass = 'xs' | 'sm' | 'md' | 'lg' | 'xl';

/**
 * Buckets `window.innerWidth` into the five classes of the protocol
 * (body §D.7).
 *
 * This is a deliberate copy of `src/server/enrich/viewport.ts`: the browser
 * bundle must not import a server file, and importing one would drag the
 * protocol vocabulary — and everything it imports — into the tracker.
 * `test/viewport.test.ts` asserts the two copies agree on every bound.
 *
 * @param width - The viewport width in CSS pixels.
 * @returns The bucket the width falls in.
 */
export function clientViewportClass(width: number): ClientViewportClass {
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
