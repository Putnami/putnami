import { describe, expect, test } from 'bun:test';
import { Divider } from '../../src/components/divider';
import { render } from './render';

describe('Divider', () => {
  test('renders structural elements with semantic horizontal values', () => {
    const html = render(<Divider orientation='horizontal' variant='dashed' />);
    expect(html).toContain('aria-orientation="horizontal"');
  });

  test('renders vertical orientation', () => {
    const html = render(<Divider orientation='vertical' />);
    expect(html).toContain('aria-orientation="vertical"');
  });
});
