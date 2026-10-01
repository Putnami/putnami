/**
 * Dismissal decision logic shared by overlay components (Dropdown, Popover).
 *
 * An open overlay closes on two interactions:
 *
 * - an outside pointerdown — a `mousedown` whose target is contained in neither the
 *   overlay surface nor the trigger;
 * - the Escape key.
 *
 * Pure helpers over a minimal `{ contains }` surface so the two dismiss rules
 * are unit-testable with stub nodes.
 */

/** Minimal shape of the DOM nodes the dismissal helpers read (kept narrow for testability). */
export interface ContainsLike {
  contains(other: unknown): boolean;
}

/**
 * Returns true when a pointer event that landed on `target` should dismiss an open
 * overlay: the click is outside both the overlay surface and its trigger.
 *
 * A null surface or trigger (the refs are not yet attached) is treated as "not
 * containing the target", matching the original inline guards which required both refs
 * to be present before closing. Pure (no globals) so the rule is unit-testable.
 */
export function isOutsideClick(target: unknown, surface: ContainsLike | null, trigger: ContainsLike | null): boolean {
  if (!surface || !trigger) return false;
  return !surface.contains(target) && !trigger.contains(target);
}

/** Returns true when a keydown should dismiss an open overlay (the Escape key). */
export function isDismissKey(key: string): boolean {
  return key === 'Escape';
}
