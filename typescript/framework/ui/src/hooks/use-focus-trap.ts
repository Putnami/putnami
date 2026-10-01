import { type RefObject, useEffect } from 'react';

/**
 * Focus management for modal dialogs (Modal, Drawer).
 *
 * A `role="dialog" aria-modal="true"` element promises assistive tech that focus
 * is *contained*: opening it moves focus inside, Tab cannot escape it, and closing
 * it returns focus where it was. `useFocusTrap` implements that contract:
 *
 * - on open, it remembers `document.activeElement` and moves focus to the first
 *   focusable element inside the container (or an explicit `[data-autofocus]`
 *   target if present);
 * - while open, a Tab keydown cycles focus within the container (wrapping last→first,
 *   and shift+Tab first→last);
 * - on close, it restores focus to the element that was focused when it opened.
 *
 * All DOM access is browser-guarded so the hook is inert during SSR
 * (`renderToStaticMarkup`): the effect body does not run on the server, and
 * the pure helpers below never touch globals.
 */

/**
 * CSS selector matching the elements that are tabbable in practice. `[href]` covers
 * anchors, the form controls cover inputs/buttons, and `[tabindex]` (filtered for
 * negative values in {@link getFocusableElements}) covers anything opted in.
 */
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]',
].join(',');

/** Minimal shape of the element APIs the hook relies on (kept narrow for testability). */
interface FocusableLike {
  getAttribute(name: string): string | null;
  hasAttribute(name: string): boolean;
}

/**
 * Returns true when `el` should be treated as focusable: it must not be explicitly
 * disabled, hidden, or removed from the tab order via a negative `tabindex`.
 */
export function isFocusable(el: FocusableLike): boolean {
  if (el.hasAttribute('disabled')) return false;
  if (el.getAttribute('aria-hidden') === 'true') return false;
  const tabindex = el.getAttribute('tabindex');
  if (tabindex != null && Number.parseInt(tabindex, 10) < 0) return false;
  return true;
}

/**
 * Given the raw match list of a container's focusable candidates, returns the ones
 * that are actually tabbable (per {@link isFocusable}), preserving document order.
 *
 * The caller in the effect passes `container.querySelectorAll(FOCUSABLE_SELECTOR)`.
 */
export function filterFocusable<T extends FocusableLike>(candidates: readonly T[]): T[] {
  return candidates.filter(isFocusable);
}

/**
 * Computes which element a Tab/Shift+Tab press should move focus to in order to keep
 * focus trapped, or `null` when the browser's default behaviour already keeps focus
 * inside the container (so the caller should not call `preventDefault`).
 *
 * Wrapping rules: tabbing off the last element wraps to the first; shift+tabbing off
 * the first wraps to the last. If the active element is not currently inside the
 * container, focus is pulled back to the first (or last, when shift) element.
 */
export function computeTrapTarget<T>(
  focusable: readonly T[],
  active: T | null,
  shiftKey: boolean,
  fallback?: T,
): T | null {
  if (focusable.length === 0) {
    return fallback ?? null;
  }

  const first = focusable[0] as T;
  const last = focusable[focusable.length - 1] as T;
  const activeIndex = active == null ? -1 : focusable.indexOf(active);

  if (activeIndex === -1) {
    // Focus escaped the container (or never entered) — pull it back to an edge.
    return shiftKey ? last : first;
  }
  if (shiftKey && active === first) return last;
  if (!shiftKey && active === last) return first;
  // Mid-list: the browser keeps focus inside on its own.
  return null;
}

/** Elements that can be programmatically focused; the subset of HTMLElement the hook uses. */
type FocusableHTMLElement = HTMLElement & { focus: (options?: FocusOptions) => void };

const queryFocusable = (container: HTMLElement): FocusableHTMLElement[] =>
  filterFocusable(Array.from(container.querySelectorAll<FocusableHTMLElement>(FOCUSABLE_SELECTOR)));

/**
 * Picks the element to focus when the dialog opens: an explicit `[data-autofocus]`
 * target inside the container if present and focusable, otherwise the first focusable
 * element, otherwise the container itself (which is given `tabindex=-1` by the hook).
 */
const initialFocusTarget = (container: HTMLElement): FocusableHTMLElement => {
  const explicit = container.querySelector<FocusableHTMLElement>('[data-autofocus]');
  if (explicit && isFocusable(explicit)) return explicit;
  const focusable = queryFocusable(container);
  return focusable[0] ?? (container as FocusableHTMLElement);
};

/**
 * Traps keyboard focus inside `ref.current` while `isOpen` is true and restores it
 * to the previously-focused element on close. Reused by both Modal and Drawer.
 *
 * Safe to call during SSR: the effect only runs in the browser, and it no-ops when
 * the ref is unmounted or `document` is unavailable.
 */
export function useFocusTrap(ref: RefObject<HTMLElement | null>, isOpen: boolean): void {
  useEffect(() => {
    if (!isOpen) return;
    if (typeof document === 'undefined') return;
    const container = ref.current;
    if (!container) return;

    // Remember where focus was so we can restore it on close.
    const previouslyFocused = document.activeElement as HTMLElement | null;

    // Containers are not focusable by default; make sure focus has somewhere to land
    // even when the dialog has no focusable children yet.
    if (!container.hasAttribute('tabindex')) {
      container.setAttribute('tabindex', '-1');
    }
    initialFocusTarget(container).focus();

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Tab') return;
      const focusable = queryFocusable(container);
      const active = (document.activeElement as FocusableHTMLElement | null) ?? null;
      const target = computeTrapTarget(focusable, active, event.shiftKey, container as FocusableHTMLElement);
      if (target) {
        event.preventDefault();
        target.focus();
      }
    };

    document.addEventListener('keydown', handleKeyDown, true);

    return () => {
      document.removeEventListener('keydown', handleKeyDown, true);
      // Restore focus to the trigger only if it is still connected to the document.
      if (previouslyFocused && typeof previouslyFocused.focus === 'function' && previouslyFocused.isConnected) {
        previouslyFocused.focus();
      }
    };
  }, [ref, isOpen]);
}
