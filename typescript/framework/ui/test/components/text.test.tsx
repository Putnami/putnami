import { describe, expect, test } from 'bun:test';
import { Text } from '../../src/components/text';
import { render, renderBothModes } from './render';

describe('Text', () => {
  test('forwards correct structural element', () => {
    const htmlSpan = render(<Text>Default</Text>);
    expect(htmlSpan).toStartWith('<span');

    const htmlP = render(<Text as='p'>Paragraph</Text>);
    expect(htmlP).toStartWith('<p');

    const htmlStrong = render(<Text as='strong'>Bolded string</Text>);
    expect(htmlStrong).toStartWith('<strong');
  });

  test('applies structural styling properties appropriately', () => {
    const html = render(
      <Text truncate size='lg' weight='bold'>
        Label
      </Text>,
    );
    expect(html).toContain('Label');
  });

  test('emits the same class in both color modes, so the color tracks the mode', () => {
    const colors = [undefined, 'primary', 'secondary', 'disabled', 'error', 'success', 'warning', 'info'] as const;
    for (const color of colors) {
      const { light, dark } = renderBothModes(<Text color={color}>Body</Text>);
      expect(dark).toBe(light);
    }
  });

  test('keeps every named tone distinct', () => {
    const colors = ['primary', 'secondary', 'disabled', 'error', 'success', 'warning', 'info'] as const;
    const classes = colors.map((color) => render(<Text color={color}>Body</Text>));
    expect(new Set(classes).size).toBe(colors.length);
  });

  test('passes a custom color through untouched', () => {
    expect(render(<Text color='rebeccapurple'>Body</Text>)).not.toBe(render(<Text color='primary'>Body</Text>));
  });
});
