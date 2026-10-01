import { describe, expect, test } from 'bun:test';
import { Progress } from '../../src/components/progress';
import { render } from './render';

describe('Progress', () => {
  test('renders standard progress with aria mapping', () => {
    const html = render(<Progress value={50} max={100} aria-label='Loading files' />);
    expect(html).toContain('role="progressbar"');
    expect(html).toContain('aria-valuenow="50"');
    expect(html).toContain('aria-valuemin="0"');
    expect(html).toContain('aria-valuemax="100"');
    expect(html).toContain('aria-label="Loading files"');
  });

  test('handles indeterminate state correctly', () => {
    const html = render(<Progress isIndeterminate />);
    // Indeterminate progress should not expose valuenow to screen readers
    expect(html).not.toContain('aria-valuenow');
    expect(html).toContain('role="progressbar"');
  });
});
