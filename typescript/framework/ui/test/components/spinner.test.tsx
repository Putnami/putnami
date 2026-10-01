import { describe, expect, test } from 'bun:test';
import { Spinner } from '../../src/components/spinner';
import { render } from './render';

describe('Spinner', () => {
  test('renders with status role', () => {
    const html = render(<Spinner size='lg' />);
    expect(html).toContain('role="status"');
    expect(html).toContain('aria-label="Loading"');
  });

  test('forwards arbitrary DOM props (id, data-*) to the root element', () => {
    const html = render(<Spinner id='load-spinner' data-testid='spinner' />);
    expect(html).toContain('id="load-spinner"');
    expect(html).toContain('data-testid="spinner"');
  });
});
