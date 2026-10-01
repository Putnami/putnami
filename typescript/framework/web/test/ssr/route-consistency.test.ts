import { describe, expect, it } from 'bun:test';
import { generatePageId } from '../../src/shared/route-tree.utils';

// We want to verify that the logic used by ReactClientGenerator and ReactApplication
// (which now both use generatePageId) produces consistent results for the same inputs.

describe('Route ID Consistency', () => {
  const scenarios = [
    { layoutId: 'root-layout', path: '', expected: 'root-page' },
    { layoutId: 'root-layout', path: 'docs', expected: 'root-docs-page' },
    { layoutId: 'root-layout', path: 'docs/[package]', expected: 'root-docs-package-page' },
    { layoutId: 'root-layout', path: 'docs/:package', expected: 'root-docs-package-page' },
    { layoutId: 'root-dashboard-layout', path: 'settings/profile', expected: 'root-dashboard-settings-profile-page' },
  ];

  it('produces consistent IDs for known scenarios', () => {
    for (const scenario of scenarios) {
      const id = generatePageId(scenario.layoutId, scenario.path);
      expect(id).toBe(scenario.expected);
    }
  });

  // Simulation of how ReactApplication uses it
  it('matches ReactApplication server-side usage', () => {
    const layoutId = 'root-layout';
    const remainingPath = 'users/:id'; // ReactApplication sees colon params

    // Server logic simulation
    const serverId = generatePageId(layoutId, remainingPath);

    expect(serverId).toBe('root-users-id-page');
  });

  // Simulation of how ReactClientGenerator uses it
  it('matches ReactClientGenerator client-side usage', () => {
    const layoutId = 'root-layout';
    const bracketPath = 'users/[id]';
    const colonPath = 'users/:id';

    // Simulate Client (brackets) vs Server (colons) inputs
    const clientId = generatePageId(layoutId, bracketPath);
    const serverId = generatePageId(layoutId, colonPath);

    // Both should produce the exact same ID with normalized dashes
    expect(clientId).toBe('root-users-id-page');
    expect(serverId).toBe('root-users-id-page');
    expect(clientId).toBe(serverId);
  });
});
