import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { generatePageId } from '../../src/shared/route-tree.utils';

describe('generatePageId', () => {
  it('generates ID for index page under root layout', () => {
    // Layout: root-layout
    // Path: '' (index)
    // Expected: root-page
    expect(generatePageId('root-layout', '')).toBe('root-page');
  });

  specTest(
    'generates ID for nested page under root layout',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'route-graph',
      check: 'a-nested-page-id-is-derived-deterministically',
    },
    () => {
      // Layout: root-layout
      // Path: docs
      // Expected: root-docs-page
      expect(generatePageId('root-layout', 'docs')).toBe('root-docs-page');
    },
  );

  it('generates ID for deep nested page', () => {
    // Layout: root-layout
    // Path: docs/api
    // Expected: root-docs-api-page
    expect(generatePageId('root-layout', 'docs/api')).toBe('root-docs-api-page');
  });

  it('generates ID with params', () => {
    // Layout: root-layout
    // Path: users/:id
    // Expected: root-users-id-page
    expect(generatePageId('root-layout', 'users/:id')).toBe('root-users-id-page');
  });

  it('generates ID from layout ID without -layout suffix', () => {
    // Layout: root (client generator sometimes passes this)
    // Path: docs
    // Expected: root-docs-page
    expect(generatePageId('root', 'docs')).toBe('root-docs-page');
  });

  it('generates ID for page under nested layout', () => {
    // Layout: root-dashboard-layout
    // Path: settings
    // Expected: root-dashboard-settings-page
    expect(generatePageId('root-dashboard-layout', 'settings')).toBe('root-dashboard-settings-page');
  });

  it('handles empty layout ID', () => {
    // Fallback behavior if layout ID is missing
    expect(generatePageId('', 'docs')).toBe('-docs-page');
  });
});
