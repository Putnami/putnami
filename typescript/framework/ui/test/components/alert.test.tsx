import { describe, expect, test } from 'bun:test';
import { Alert } from '../../src/components/alert';
import { render } from './render';

describe('Alert', () => {
  test('renders with status roles', () => {
    const html = render(<Alert status='error' title='Warning' />);
    // Checking the structural component role
    expect(html).toContain('role="alert"');
    expect(html).toContain('Warning');

    // Check close wrapper maps aria-label
    const closableHtml = render(<Alert onClose={() => {}} />);
    expect(closableHtml).toContain('aria-label="Close alert"');
  });

  test('forwards arbitrary DOM props (id, data-*) to the root element', () => {
    const html = render(<Alert id='session-alert' data-testid='alert' status='info' title='Heads up' />);
    expect(html).toContain('id="session-alert"');
    expect(html).toContain('data-testid="alert"');
  });
});
