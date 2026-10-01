import { describe, expect, test } from 'bun:test';
import { Page } from '../../src/layout/page';
import { render, renderBothModes } from '../components/render';

describe('Page', () => {
  test('renders a div wrapper around its children', () => {
    const html = render(<Page>Content</Page>);
    expect(html).toStartWith('<div');
    expect(html).toContain('Content');
  });

  test('emits the same class in both color modes, so background and text track the mode', () => {
    const { light, dark } = renderBothModes(<Page>Content</Page>);
    expect(dark).toBe(light);
  });
});
