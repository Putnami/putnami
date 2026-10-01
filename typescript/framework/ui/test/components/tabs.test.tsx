import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { createElement, Fragment, type ReactNode } from 'react';
import { nextEnabledTabIndex, Tab, TabList, TabPanel, TabPanels, Tabs } from '../../src/components/tabs';
import { render } from './render';

// Build a Fragment dynamically so Biome's noUselessFragments lint can't strip it
// at lint time — this lets us exercise the real Fragment-handling code path.
const fragmentOf = (...children: ReactNode[]) => createElement(Fragment, null, ...children);

describe('Tabs', () => {
  specTest(
    'tablist, tab, tabpanel roles are rendered',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'tabs-publish-tablist-tab-and-tabpanel-roles',
    },
    () => {
      const html = render(
        <Tabs>
          <TabList>
            <Tab>One</Tab>
            <Tab>Two</Tab>
          </TabList>
          <TabPanels>
            <TabPanel>Panel One</TabPanel>
            <TabPanel>Panel Two</TabPanel>
          </TabPanels>
        </Tabs>,
      );

      expect(html).toContain('role="tablist"');
      expect(html).toContain('role="tab"');
      expect(html).toContain('role="tabpanel"');
    },
  );

  specTest(
    'selected tab and hidden panel state follow defaultIndex',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'tabs-publish-their-selection-state',
    },
    () => {
      const html = render(
        <Tabs defaultIndex={1}>
          <TabList>
            <Tab>One</Tab>
            <Tab>Two</Tab>
          </TabList>
          <TabPanels>
            <TabPanel>Panel One</TabPanel>
            <TabPanel>Panel Two</TabPanel>
          </TabPanels>
        </Tabs>,
      );

      // Selected tab receives aria-selected="true"
      expect(html).toContain('aria-selected="true"');

      // Inactive tab receives aria-selected="false"
      expect(html).toContain('aria-selected="false"');

      // Hidden panel receives hidden=""
      expect(html).toContain('hidden=""');
    },
  );

  test('using Tab outside Tabs throws expected error', () => {
    expect(() => render(<Tab>Lone Tab</Tab>)).toThrow('Tabs components must be used within a Tabs provider');
  });

  test('indices are assigned when children are wrapped in a Fragment', () => {
    const html = render(
      <Tabs defaultIndex={1}>
        <TabList>{fragmentOf(<Tab key='1'>One</Tab>, <Tab key='2'>Two</Tab>)}</TabList>
        <TabPanels>
          {fragmentOf(<TabPanel key='1'>Panel One</TabPanel>, <TabPanel key='2'>Panel Two</TabPanel>)}
        </TabPanels>
      </Tabs>,
    );

    // Second tab is selected (index 1) — confirms indexing worked through the Fragment.
    const selectedTrueCount = (html.match(/aria-selected="true"/g) ?? []).length;
    const selectedFalseCount = (html.match(/aria-selected="false"/g) ?? []).length;
    expect(selectedTrueCount).toBe(1);
    expect(selectedFalseCount).toBe(1);
    // The second panel is visible.
    expect(html).toContain('Panel Two');
  });

  test('indices are assigned when children are nested in multiple Fragments', () => {
    // Deeper nesting: a Fragment containing a Fragment. flattenChildren must recurse.
    const inner = fragmentOf(<Tab key='1'>One</Tab>, <Tab key='2'>Two</Tab>);
    const html = render(
      <Tabs defaultIndex={2}>
        <TabList>{fragmentOf(inner, <Tab key='3'>Three</Tab>)}</TabList>
        <TabPanels>
          <TabPanel>Panel One</TabPanel>
          <TabPanel>Panel Two</TabPanel>
          <TabPanel>Panel Three</TabPanel>
        </TabPanels>
      </Tabs>,
    );

    const selectedTrueCount = (html.match(/aria-selected="true"/g) ?? []).length;
    const selectedFalseCount = (html.match(/aria-selected="false"/g) ?? []).length;
    expect(selectedTrueCount).toBe(1);
    expect(selectedFalseCount).toBe(2);
  });

  test('indices are assigned when children come from Array.prototype.map', () => {
    const labels = ['One', 'Two', 'Three'];
    const html = render(
      <Tabs defaultIndex={2}>
        <TabList>
          {labels.map((label) => (
            <Tab key={label}>{label}</Tab>
          ))}
        </TabList>
        <TabPanels>
          {labels.map((label) => (
            <TabPanel key={label}>Panel {label}</TabPanel>
          ))}
        </TabPanels>
      </Tabs>,
    );

    // Third tab (index 2) is selected → mapping produced unique indices.
    const selectedTrueCount = (html.match(/aria-selected="true"/g) ?? []).length;
    const selectedFalseCount = (html.match(/aria-selected="false"/g) ?? []).length;
    expect(selectedTrueCount).toBe(1);
    expect(selectedFalseCount).toBe(2);
    expect(html).toContain('Panel Three');
  });

  test('mixed direct + mapped children produce monotonically increasing indices', () => {
    // The mapped children sit between two direct Tabs — every Tab still gets a unique
    // index, so exactly one is selected at defaultIndex.
    const middleLabels = ['Beta', 'Gamma'];
    const html = render(
      <Tabs defaultIndex={3}>
        <TabList>
          <Tab>Alpha</Tab>
          {middleLabels.map((label) => (
            <Tab key={label}>{label}</Tab>
          ))}
          <Tab>Delta</Tab>
        </TabList>
        <TabPanels>
          <TabPanel>Alpha Panel</TabPanel>
          {middleLabels.map((label) => (
            <TabPanel key={label}>{label} Panel</TabPanel>
          ))}
          <TabPanel>Delta Panel</TabPanel>
        </TabPanels>
      </Tabs>,
    );

    const selectedTrueCount = (html.match(/aria-selected="true"/g) ?? []).length;
    const selectedFalseCount = (html.match(/aria-selected="false"/g) ?? []).length;
    expect(selectedTrueCount).toBe(1);
    expect(selectedFalseCount).toBe(3);
    // Delta is at index 3 (Alpha=0, Beta=1, Gamma=2, Delta=3).
    expect(html).toContain('Delta Panel');
  });

  specTest(
    'roving tabIndex is rendered so arrow keys have somewhere to move focus',
    { feature: 'typescript/ui-system', requirement: 'interactive-semantics', check: 'tabs-publish-a-roving-tabindex' },
    () => {
      // The keyboard handler relies on the roving tabindex: only the active tab is
      // tabIndex=0, every other tab is tabIndex=-1 and thus reachable solely via arrows.
      const html = render(
        <Tabs defaultIndex={0}>
          <TabList>
            <Tab>One</Tab>
            <Tab>Two</Tab>
          </TabList>
          <TabPanels>
            <TabPanel>Panel One</TabPanel>
            <TabPanel>Panel Two</TabPanel>
          </TabPanels>
        </Tabs>,
      );

      expect((html.match(/tabindex="0"/g) ?? []).length).toBe(1);
      expect((html.match(/tabindex="-1"/g) ?? []).length).toBe(1);
    },
  );
});

/**
 * The arrow-key handler itself moves DOM focus, which needs a real browser (the bun
 * test env has no element tree / activeElement and a DOM env cannot be added without a
 * new dependency). `nextEnabledTabIndex` is the pure decision core the handler delegates
 * to — given the per-index disabled flags and the focused tab, it returns the index the
 * next/previous/Home/End key should activate — so the navigation rule is fully testable.
 */
describe('nextEnabledTabIndex', () => {
  const noneDisabled = [false, false, false];

  specTest(
    'ArrowRight moves to the next tab',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'arrow-right-moves-to-the-next-tab',
    },
    () => {
      expect(nextEnabledTabIndex(noneDisabled, 0, 'ArrowRight')).toBe(1);
      expect(nextEnabledTabIndex(noneDisabled, 1, 'ArrowRight')).toBe(2);
    },
  );

  specTest(
    'ArrowLeft moves to the previous tab',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'arrow-left-moves-to-the-previous-tab',
    },
    () => {
      expect(nextEnabledTabIndex(noneDisabled, 2, 'ArrowLeft')).toBe(1);
      expect(nextEnabledTabIndex(noneDisabled, 1, 'ArrowLeft')).toBe(0);
    },
  );

  specTest(
    'ArrowRight wraps from the last tab to the first',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'arrow-navigation-wraps-past-the-last-tab',
    },
    () => {
      expect(nextEnabledTabIndex(noneDisabled, 2, 'ArrowRight')).toBe(0);
    },
  );

  test('ArrowLeft wraps from the first tab to the last', () => {
    expect(nextEnabledTabIndex(noneDisabled, 0, 'ArrowLeft')).toBe(2);
  });

  specTest(
    'Home selects the first enabled tab, End the last',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'home-and-end-select-the-first-and-last-enabled-tab',
    },
    () => {
      expect(nextEnabledTabIndex(noneDisabled, 2, 'Home')).toBe(0);
      expect(nextEnabledTabIndex(noneDisabled, 0, 'End')).toBe(2);
    },
  );

  specTest(
    'ArrowRight skips a disabled tab',
    {
      feature: 'typescript/ui-system',
      requirement: 'keyboard-and-dismissal',
      check: 'arrow-navigation-skips-a-disabled-tab',
    },
    () => {
      // index 1 disabled → from 0 the next enabled is 2.
      expect(nextEnabledTabIndex([false, true, false], 0, 'ArrowRight')).toBe(2);
    },
  );

  test('ArrowLeft skips a disabled tab while wrapping', () => {
    // index 0 disabled → from 1 the previous enabled wraps past 0 to 2.
    expect(nextEnabledTabIndex([true, false, false], 1, 'ArrowLeft')).toBe(2);
  });

  test('Home/End skip disabled tabs at the edges', () => {
    expect(nextEnabledTabIndex([true, false, true], 1, 'Home')).toBe(1);
    expect(nextEnabledTabIndex([true, false, true], 1, 'End')).toBe(1);
  });

  test('stays put / returns null when there is no other enabled tab to move to', () => {
    // Only index 1 is enabled and it is already current: the wrap lands back on 1, which
    // the Tab handler treats as a no-op (target === index).
    expect(nextEnabledTabIndex([true, false, true], 1, 'ArrowRight')).toBe(1);
    // Every tab disabled → nothing to move to.
    expect(nextEnabledTabIndex([true, true, true], 0, 'ArrowRight')).toBeNull();
    // Empty tablist.
    expect(nextEnabledTabIndex([], 0, 'Home')).toBeNull();
  });

  test('returns null for non-navigation keys', () => {
    expect(nextEnabledTabIndex(noneDisabled, 0, 'Enter')).toBeNull();
    expect(nextEnabledTabIndex(noneDisabled, 0, 'a')).toBeNull();
    expect(nextEnabledTabIndex(noneDisabled, 0, 'ArrowDown')).toBeNull();
  });
});
