import React from 'react';
import { hydrateRoot } from 'react-dom/client';
import {
  type IslandComponent,
  ISLAND_ID_ATTR,
  ISLAND_MEDIA_ATTR,
  ISLAND_PROPS_ATTR,
  ISLAND_ROOT_TAG,
  ISLAND_SLOT_TAG,
  ISLAND_STRATEGY_ATTR,
  ISLAND_TAG,
  type IslandStrategy,
  parseIslandProps,
} from './island-types';
import { IslandModeContext, SlotHtmlContext } from './island';

/** Lazy loader for an island module (its default export is the island host). */
export type IslandLoader = () => Promise<{ default: IslandComponent }>;

/** Map of island id -> lazy loader, emitted by the build. */
export type IslandMap = Record<string, IslandLoader>;

const HYDRATED = new WeakSet<Element>();

/**
 * Schedule `run` according to the island's hydration `strategy`. Returns a
 * teardown for any registered observers/listeners.
 */
export function scheduleHydration(
  element: Element,
  strategy: IslandStrategy,
  media: string | undefined,
  run: () => void,
): () => void {
  switch (strategy) {
    case 'idle': {
      const ric = (globalThis as { requestIdleCallback?: (cb: () => void) => number }).requestIdleCallback;
      if (ric) {
        ric(run);
      } else {
        setTimeout(run, 1);
      }
      return () => {};
    }
    case 'visible': {
      if (typeof IntersectionObserver === 'undefined') {
        run();
        return () => {};
      }
      const observer = new IntersectionObserver((entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            observer.disconnect();
            run();
            return;
          }
        }
      });
      observer.observe(element);
      return () => observer.disconnect();
    }
    case 'media': {
      if (!media || typeof matchMedia === 'undefined') {
        run();
        return () => {};
      }
      const mql = matchMedia(media);
      if (mql.matches) {
        run();
        return () => {};
      }
      const listener = (event: MediaQueryListEvent) => {
        if (event.matches) {
          mql.removeEventListener('change', listener);
          run();
        }
      };
      mql.addEventListener('change', listener);
      return () => mql.removeEventListener('change', listener);
    }
    default:
      run();
      return () => {};
  }
}

/**
 * Hydrate a single island marker element using the provided host component.
 * Reads serialized props and preserves slotted static HTML.
 */
export function hydrateIslandElement(marker: Element, host: IslandComponent): void {
  if (HYDRATED.has(marker)) return;
  const root = marker.querySelector(ISLAND_ROOT_TAG);
  if (!root) return;

  const propsScript = marker.querySelector(`script[${ISLAND_PROPS_ATTR}]`);
  const props = parseIslandProps(propsScript?.textContent);
  const slot = root.querySelector(ISLAND_SLOT_TAG);
  const slotHtml = slot ? slot.innerHTML : undefined;

  const component = host.__island.component;
  HYDRATED.add(marker);

  hydrateRoot(
    root,
    React.createElement(
      IslandModeContext.Provider,
      { value: 'hydrate' as const },
      React.createElement(SlotHtmlContext.Provider, { value: slotHtml }, React.createElement(component, props)),
    ),
  );
}

/**
 * Scan the document for island markers and hydrate each one independently,
 * loading its bundle on demand per its hydration strategy. Islands not present
 * in the DOM are never loaded, so a static page ships only the JS it uses.
 */
export async function runIslands(islandMap: IslandMap): Promise<void> {
  if (typeof document === 'undefined') return;
  const markers = Array.from(document.querySelectorAll(ISLAND_TAG));

  for (const marker of markers) {
    const id = marker.getAttribute(ISLAND_ID_ATTR);
    if (!id) continue;
    const loader = islandMap[id];
    if (!loader) continue;

    const strategy = (marker.getAttribute(ISLAND_STRATEGY_ATTR) as IslandStrategy) || 'load';
    const media = marker.getAttribute(ISLAND_MEDIA_ATTR) ?? undefined;

    scheduleHydration(marker, strategy, media, () => {
      loader()
        .then((mod) => hydrateIslandElement(marker, mod.default))
        .catch((error) => {
          // biome-ignore lint/suspicious/noConsole: client-side island error reporting
          console.error('[putnami:island] failed to hydrate', id, error);
        });
    });
  }
}
