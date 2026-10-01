import { describe, expect, it } from 'bun:test';
import { ErrorBoundary } from '../../src/client/error/error-boundary';

describe('ErrorBoundary', () => {
  it('is exported as a function', () => {
    expect(typeof ErrorBoundary).toBe('function');
  });

  // Note: Testing actual rendering requires a DOM environment and React Router context
  // which is complex to set up in Bun test environment currently.
});
