import type { ColorMode, ResolvedColorMode } from './theme';

/**
 * Color-mode decisions shared by `ThemeProvider`, written as pure helpers
 * over plain values so they are unit-testable without a DOM.
 */

/** The `localStorage` key under which the user's color-mode preference is persisted. */
export const STORAGE_KEY = 'putnami-color-mode';

/** Narrows an arbitrary value to a valid `ColorMode`. */
export function isColorMode(value: unknown): value is ColorMode {
  return value === 'light' || value === 'dark' || value === 'system';
}

/**
 * Validates a raw value read from storage, returning a `ColorMode` or `null`.
 *
 * Pure (takes the already-read string) so the validation can be tested without a
 * `localStorage` global: anything that is not one of the three known modes — including
 * `null` or `''` — is rejected.
 */
export function parseStoredColorMode(raw: string | null): ColorMode | null {
  return isColorMode(raw) ? raw : null;
}

/** Resolves the user's preference against the OS setting: `'system'` follows `systemMode`. */
export function resolveColorMode(colorMode: ColorMode, systemMode: ResolvedColorMode): ResolvedColorMode {
  return colorMode === 'system' ? systemMode : colorMode;
}

/**
 * Builds the blocking inline script injected into `<head>` to apply the saved color
 * mode before first paint (avoiding a flash of the wrong palette).
 *
 * It reads the persisted preference, falls back to `'system'` (and then to the OS's
 * `prefers-color-scheme`), and writes `data-color-mode` + `color-scheme` onto
 * `<html>`. Returned as a string so its contents — that it reads `storageKey`, honours
 * the media query, and sets both attributes — can be asserted in a unit test.
 */
export function buildColorModeScript(storageKey: string = STORAGE_KEY): string {
  return [
    '(function(){',
    `var s=${JSON.stringify(storageKey)};`,
    "try{var m=localStorage.getItem(s)||'system'}catch(e){var m='system'}",
    "if(m==='system'){m=matchMedia('(prefers-color-scheme:dark)').matches?'dark':'light'}",
    'document.documentElement.dataset.colorMode=m;',
    'document.documentElement.style.colorScheme=m;',
    '})()',
  ].join('');
}

/**
 * Detects an already-injected color-mode script among `DocumentMeta.scripts`, so the
 * provider stays idempotent across re-renders and nested providers. Matches on the
 * storage key the script is guaranteed to reference.
 */
export function isColorModeScript(script: { children?: unknown }): boolean {
  return typeof script.children === 'string' && script.children.includes(STORAGE_KEY);
}
