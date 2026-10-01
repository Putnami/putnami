import { describe, expect, test } from 'bun:test';
import { MarkdownContent } from '../../src/components/markdown-content';
import { render } from './render';

describe('MarkdownContent', () => {
  test('renders children successfully', () => {
    const html = render(
      <MarkdownContent>
        <h1>Heading 1</h1>
        <p>Paragraph</p>
      </MarkdownContent>,
    );

    expect(html).toContain('<h1>Heading 1</h1>');
    expect(html).toContain('<p>Paragraph</p>');
  });

  test('renders code blocks successfully', () => {
    const html = render(
      <MarkdownContent>
        <pre>
          <code>const value = 42;</code>
        </pre>
      </MarkdownContent>,
    );

    expect(html).toContain('<pre>');
    expect(html).toContain('const value = 42;');
  });
});
