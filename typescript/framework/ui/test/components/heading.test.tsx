import { describe, expect, test } from 'bun:test';
import { Heading } from '../../src/components/heading';
import { render, renderBothModes } from './render';

describe('Heading', () => {
  test('renders correct default heading element mapping', () => {
    // level 2 should be h2
    const htmlTwo = render(<Heading level={2}>Title</Heading>);
    expect(htmlTwo).toStartWith('<h2');
    expect(htmlTwo).toContain('Title</h2>');

    // level 1 should be h1
    const htmlOne = render(<Heading level={1}>Title</Heading>);
    expect(htmlOne).toStartWith('<h1');
  });

  test('overrides structural element but keeps visually sized headings', () => {
    const html = render(
      <Heading as='h4' level={1}>
        Semantic small, visually large
      </Heading>,
    );
    expect(html).toStartWith('<h4');
  });

  test('renders text content with truncation class', () => {
    const html = render(<Heading truncate>Long text</Heading>);
    expect(html).toContain('Long text');
  });

  test('emits the same class in both color modes, so the color tracks the mode', () => {
    for (const color of [undefined, 'primary', 'secondary', 'disabled'] as const) {
      const { light, dark } = renderBothModes(<Heading color={color}>Title</Heading>);
      expect(dark).toBe(light);
    }
  });

  test('keeps the three text tones distinct', () => {
    const classes = (['primary', 'secondary', 'disabled'] as const).map((color) =>
      render(<Heading color={color}>Title</Heading>),
    );
    expect(new Set(classes).size).toBe(3);
  });

  test('passes a custom color through untouched', () => {
    const html = render(<Heading color='rebeccapurple'>Title</Heading>);
    expect(html).not.toBe(render(<Heading>Title</Heading>));
  });
});
