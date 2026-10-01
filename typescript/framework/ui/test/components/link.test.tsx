import { describe, expect, test } from 'bun:test';
import { Link } from '../../src/components/link';
import { render, renderBothModes } from './render';

describe('Link', () => {
  test('renders with href and forwards styles', () => {
    const html = render(<Link to='/about'>About Us</Link>);
    expect(html).toContain('href="/about"');
    expect(html).toContain('About Us');
  });

  test('emits the same class in both color modes, so the color tracks the mode', () => {
    const { light, dark } = renderBothModes(<Link to='/about'>About Us</Link>);
    expect(dark).toBe(light);
  });
});
