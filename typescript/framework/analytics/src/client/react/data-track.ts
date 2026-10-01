import { bounded, MAX_PROP_KEYS, MAX_PROP_STRING_LEN, PROP_KEY_RE } from '../bounds';
import type { TrackFn } from '../wire';

/** The attribute that names the action, and the prefix that carries its props. */
const NAME_ATTRIBUTE = 'data-track';
const PROP_PREFIX = 'data-track-';

/** Reads `data-track-*` attributes into a bounded, protocol-shaped props object. */
function propsOf(element: Element): Record<string, string> {
  const props: Record<string, string> = {};
  for (const attribute of Array.from(element.attributes)) {
    if (!attribute.name.startsWith(PROP_PREFIX) || attribute.name.length <= PROP_PREFIX.length) {
      continue;
    }
    const key = attribute.name.slice(PROP_PREFIX.length).replace(/-/g, '_');
    // A key the protocol rejects would take the whole event down with it, so
    // an unusable attribute is skipped rather than sent.
    if (!PROP_KEY_RE.test(key) || Object.keys(props).length >= MAX_PROP_KEYS) {
      continue;
    }
    props[key] = bounded(attribute.value, MAX_PROP_STRING_LEN);
  }
  return props;
}

/**
 * Installs the declarative click delegate.
 *
 * One capturing listener on the document serves every `data-track` element on
 * the page, including elements React mounts later: markup that appears after
 * install is tracked without re-registering anything. Capture phase, so a
 * handler that stops propagation does not silently disable analytics; passive,
 * so the listener can never delay a click.
 *
 * ```tsx
 * <button data-track="signup_click" data-track-plan="pro">Sign up</button>
 * ```
 *
 * @param track - The tracking function to call with the parsed action.
 */
export function installDataTrack(track: TrackFn): () => void {
  const listener = (event: Event): void => {
    const element = (event.target as Element | null)?.closest?.(`[${NAME_ATTRIBUTE}]`);
    if (!element) {
      return;
    }
    const name = element.getAttribute(NAME_ATTRIBUTE) ?? '';
    if (name) {
      track(name, propsOf(element));
    }
  };
  document.addEventListener('click', listener, { capture: true, passive: true });
  return () => document.removeEventListener('click', listener, { capture: true });
}
