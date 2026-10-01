import type React from 'react';

/**
 * When an island hydrates on the client.
 *
 * - `load` — hydrate immediately on page load.
 * - `idle` — hydrate during the first idle period (`requestIdleCallback`).
 * - `visible` — hydrate when the island scrolls into view (`IntersectionObserver`).
 * - `media` — hydrate when a media query matches (`matchMedia`).
 */
export type IslandStrategy = 'load' | 'idle' | 'visible' | 'media';

// Custom element + attribute names used to mark and locate islands in the DOM.
export const ISLAND_TAG = 'putnami-island';
export const ISLAND_ROOT_TAG = 'putnami-island-root';
export const ISLAND_SLOT_TAG = 'putnami-island-slot';
export const ISLAND_ID_ATTR = 'data-island';
export const ISLAND_STRATEGY_ATTR = 'data-strategy';
export const ISLAND_MEDIA_ATTR = 'data-media';
export const ISLAND_PROPS_ATTR = 'data-island-props';

/** Internal configuration carried by an island component. */
export interface IslandConfig {
  strategy: IslandStrategy;
  media?: string;
  // biome-ignore lint/suspicious/noExplicitAny: island wraps an arbitrary component
  component: React.ComponentType<any>;
  id?: string;
}

/** An island component produced by `island().render(Component)`. */
export type IslandComponent<P = Record<string, unknown>> = React.ComponentType<P> & {
  readonly __island: IslandConfig;
};

export function isIslandComponent(value: unknown): value is IslandComponent {
  return typeof value === 'function' && '__island' in (value as object);
}

// U+2028 / U+2029 break naive JSON-in-<script> embedding; match them by escape.
const LINE_SEP = /\u2028/g;
const PARA_SEP = /\u2029/g;

/**
 * Serialize island props into JSON safe to embed inside a `<script>` element.
 * Escapes the sequences that could otherwise break out of the script context
 * (`</script>`) or trigger charset-based XSS (U+2028 / U+2029).
 */
export function serializeIslandProps(props: unknown): string {
  return JSON.stringify(props ?? {})
    .replace(/</g, '\\u003c')
    .replace(/>/g, '\\u003e')
    .replace(LINE_SEP, '\\u2028')
    .replace(PARA_SEP, '\\u2029');
}

/** Parse serialized island props, tolerating missing/invalid payloads. */
export function parseIslandProps(text: string | null | undefined): Record<string, unknown> {
  if (!text) return {};
  try {
    return JSON.parse(text) as Record<string, unknown>;
  } catch {
    return {};
  }
}

/**
 * Derive a stable island id from a `*.island.tsx` file path (relative to the
 * scan root). `widgets/Counter.island.tsx` -> `widgets/Counter`.
 */
export function islandIdFromFile(file: string): string {
  return file
    .replace(/\\/g, '/')
    .replace(/\.island\.(tsx|jsx|ts|js)$/, '')
    .replace(/^\.?\//, '');
}
