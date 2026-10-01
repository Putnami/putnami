import { describe, expect, test } from 'bun:test';
import { Input } from '../../src/components/input';
import { render } from './render';

describe('Input', () => {
  test('renders with placeholder and defaults', () => {
    const html = render(<Input placeholder='Enter text' />);
    expect(html).toContain('placeholder="Enter text"');
  });

  test('handles invalid state aria mapping', () => {
    const html = render(<Input isInvalid />);
    expect(html).toContain('aria-invalid="true"');
  });

  test('handles disabled state', () => {
    const html = render(<Input isDisabled />);
    expect(html).toContain('disabled=""');
  });

  test('renders left and right elements', () => {
    const html = render(<Input leftElement={<span>Left</span>} rightElement={<span>Right</span>} />);
    expect(html).toContain('Left');
    expect(html).toContain('Right');
  });
});
