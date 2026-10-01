import React, { createContext, useContext } from 'react';
import {
  type IslandComponent,
  type IslandConfig,
  ISLAND_ID_ATTR,
  ISLAND_MEDIA_ATTR,
  ISLAND_PROPS_ATTR,
  ISLAND_ROOT_TAG,
  ISLAND_SLOT_TAG,
  ISLAND_STRATEGY_ATTR,
  ISLAND_TAG,
  type IslandStrategy,
  isIslandComponent,
  serializeIslandProps,
} from './island-types';

/**
 * Rendering mode for an island host.
 *
 * - `static` — emit the full hydration marker (`<putnami-island>`) so the
 *   client can hydrate this boundary independently. Used by static pages.
 * - `hydrate` — render the component inline; the surrounding full-page
 *   hydration already covers it. Used by SSR pages and on the client.
 */
export const IslandModeContext = createContext<'static' | 'hydrate'>('hydrate');

/** Server-side slot children, provided by an island host. */
const SlotChildrenContext = createContext<React.ReactNode>(null);

/** Client-side slot HTML, captured from the server DOM before hydration. */
export const SlotHtmlContext = createContext<string | undefined>(undefined);

// host component -> stable id, assigned by the build via registerIslandId.
const islandIds = new WeakMap<object, string>();

/**
 * Associate an island component with its build-time id so the SSR marker and
 * the client island map agree. Called from the generated SSR module.
 */
export function registerIslandId(component: IslandComponent | object, id: string): void {
  islandIds.set(component, id);
  const config = (component as { __island?: IslandConfig }).__island;
  if (config) config.id = id;
}

/** Read the registered id for an island component, if any. */
export function getIslandId(component: object): string | undefined {
  return islandIds.get(component);
}

/**
 * Placeholder for static (non-interactive) children nested inside an island.
 *
 * On the server it renders the children passed to the island; on the client it
 * re-inserts the captured server HTML so slotted content is preserved without
 * shipping its JavaScript.
 */
export function Slot(): React.ReactElement {
  const mode = useContext(IslandModeContext);
  const children = useContext(SlotChildrenContext);
  const html = useContext(SlotHtmlContext);

  if (mode === 'static') {
    return React.createElement(ISLAND_SLOT_TAG, null, children);
  }
  return React.createElement(ISLAND_SLOT_TAG, {
    suppressHydrationWarning: true,
    ...(html !== undefined ? { dangerouslySetInnerHTML: { __html: html } } : {}),
  });
}

/**
 * Fluent builder for a hydration island.
 *
 * @example
 * // hydrate when scrolled into view
 * export default island().load('visible').render(Counter);
 *
 * @example
 * // hydrate only on wide viewports
 * export default island().media('(min-width: 768px)').render(Sidebar);
 */
export class IslandBuilder {
  private _strategy: IslandStrategy = 'load';
  private _media?: string;

  /** Set the hydration strategy (`load` | `idle` | `visible` | `media`). */
  load(strategy: IslandStrategy = 'load'): this {
    this._strategy = strategy;
    return this;
  }

  /** Hydrate when the given media query matches (implies the `media` strategy). */
  media(query: string): this {
    this._strategy = 'media';
    this._media = query;
    return this;
  }

  /** Finalise the island with the component to hydrate. */
  render<P extends Record<string, unknown>>(component: React.ComponentType<P>): IslandComponent<P> {
    const config: IslandConfig = {
      strategy: this._strategy,
      ...(this._media ? { media: this._media } : {}),
      component: component as React.ComponentType,
    };

    const host = (props: P): React.ReactElement => {
      const mode = useContext(IslandModeContext);
      const { children, ...rest } = props as P & { children?: React.ReactNode };

      const inner = React.createElement(
        SlotChildrenContext.Provider,
        { value: children ?? null },
        React.createElement(component, rest as P),
      );

      if (mode !== 'static') {
        return inner;
      }

      const id = islandIds.get(host) ?? config.id ?? component.displayName ?? component.name ?? 'island';
      const attrs: Record<string, string> = {
        [ISLAND_ID_ATTR]: id,
        [ISLAND_STRATEGY_ATTR]: config.strategy,
      };
      if (config.media) attrs[ISLAND_MEDIA_ATTR] = config.media;

      return React.createElement(
        ISLAND_TAG,
        attrs,
        React.createElement(ISLAND_ROOT_TAG, null, inner),
        React.createElement('script', {
          type: 'application/json',
          [ISLAND_PROPS_ATTR]: '',
          dangerouslySetInnerHTML: { __html: serializeIslandProps(rest) },
        }),
      );
    };

    const island = host as unknown as IslandComponent<P>;
    (island as { __island: IslandConfig }).__island = config;
    return island;
  }
}

/**
 * Declare a hydration island — a component that ships and hydrates its own
 * JavaScript independently of the page. Export the result as the default export
 * of a `*.island.tsx` file.
 */
export function island(): IslandBuilder {
  return new IslandBuilder();
}

/** True when a value is an island host component. */
export function isIslandHost(value: unknown): value is IslandComponent {
  return isIslandComponent(value);
}
