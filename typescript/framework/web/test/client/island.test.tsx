import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import {
  getIslandId,
  island,
  IslandBuilder,
  IslandModeContext,
  isIslandHost,
  registerIslandId,
  Slot,
} from '../../src/client/island/island';
import {
  islandIdFromFile,
  isIslandComponent,
  parseIslandProps,
  serializeIslandProps,
} from '../../src/client/island/island-types';

function Counter(props: { count?: number }) {
  return React.createElement('button', { type: 'button' }, `count: ${props.count ?? 0}`);
}

describe('island() builder', () => {
  it('returns an IslandBuilder', () => {
    expect(island()).toBeInstanceOf(IslandBuilder);
  });

  it('defaults to the load strategy', () => {
    const def = island().render(Counter);
    expect(def.__island.strategy).toBe('load');
    expect(isIslandHost(def)).toBe(true);
    expect(isIslandComponent(def)).toBe(true);
  });

  specTest(
    'sets the strategy via load()',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'an-island-declares-its-load-strategy',
    },
    () => {
      const def = island().load('visible').render(Counter);
      expect(def.__island.strategy).toBe('visible');
    },
  );

  specTest(
    'sets the media strategy via media()',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'an-island-declares-a-media-strategy',
    },
    () => {
      const def = island().media('(min-width: 768px)').render(Counter);
      expect(def.__island.strategy).toBe('media');
      expect(def.__island.media).toBe('(min-width: 768px)');
    },
  );

  it('keeps a reference to the wrapped component', () => {
    const def = island().render(Counter);
    expect(def.__island.component).toBe(Counter);
  });
});

describe('island id registry', () => {
  it('registers and reads an id', () => {
    const def = island().render(Counter);
    registerIslandId(def, 'widgets/Counter');
    expect(getIslandId(def)).toBe('widgets/Counter');
    expect(def.__island.id).toBe('widgets/Counter');
  });
});

describe('island SSR marker', () => {
  specTest(
    'emits a hydration marker in static mode',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'a-static-page-emits-a-hydration-marker-instead-of-a-full-bundle',
    },
    () => {
      const def = island().load('visible').render(Counter);
      registerIslandId(def, 'widgets/Counter');

      const html = renderToStaticMarkup(
        React.createElement(
          IslandModeContext.Provider,
          { value: 'static' as const },
          React.createElement(def, { count: 3 }),
        ),
      );

      expect(html).toContain('<putnami-island');
      expect(html).toContain('data-island="widgets/Counter"');
      expect(html).toContain('data-strategy="visible"');
      expect(html).toContain('<putnami-island-root>');
      expect(html).toContain('count: 3');
      expect(html).toContain('data-island-props');
      // The serialized props are embedded for client hydration.
      expect(html).toContain('"count":3');
    },
  );

  specTest(
    'renders the component inline in hydrate mode (no marker)',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'a-hydrate-mode-page-renders-the-component-inline',
    },
    () => {
      const def = island().render(Counter);
      const html = renderToStaticMarkup(React.createElement(def, { count: 7 }));
      expect(html).not.toContain('<putnami-island');
      expect(html).toContain('count: 7');
    },
  );

  it('renders slotted children inside the slot element (static mode)', () => {
    function Card(_props: Record<string, unknown>) {
      return React.createElement('section', null, React.createElement(Slot));
    }
    const def = island().render(Card);
    registerIslandId(def, 'Card');

    const html = renderToStaticMarkup(
      React.createElement(
        IslandModeContext.Provider,
        { value: 'static' as const },
        React.createElement(def, null, React.createElement('p', null, 'slotted')),
      ),
    );
    expect(html).toContain('<putnami-island-slot>');
    expect(html).toContain('slotted');
  });
});

describe('island prop serialization', () => {
  specTest(
    'round-trips props',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'island-props-round-trip-through-the-boundary',
    },
    () => {
      const json = serializeIslandProps({ a: 1, b: 'x', c: [true, null] });
      expect(parseIslandProps(json)).toEqual({ a: 1, b: 'x', c: [true, null] });
    },
  );

  specTest(
    'escapes script-breaking sequences',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'island-props-escape-script-breaking-sequences',
    },
    () => {
      const json = serializeIslandProps({ html: '</script>' });
      expect(json).not.toContain('</script>');
      expect(json).toContain('\\u003c');
      expect(parseIslandProps(json)).toEqual({ html: '</script>' });
    },
  );

  it('tolerates empty / invalid payloads', () => {
    expect(parseIslandProps(undefined)).toEqual({});
    expect(parseIslandProps('not json')).toEqual({});
  });
});

describe('islandIdFromFile', () => {
  it('strips the island suffix and leading slash', () => {
    expect(islandIdFromFile('widgets/Counter.island.tsx')).toBe('widgets/Counter');
    expect(islandIdFromFile('/Counter.island.tsx')).toBe('Counter');
    expect(islandIdFromFile('Counter.island.jsx')).toBe('Counter');
  });
});
