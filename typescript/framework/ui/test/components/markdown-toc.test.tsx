import { describe, expect, test } from 'bun:test';
import { MarkdownToc } from '../../src/components/markdown-toc';
import { render } from './render';

describe('MarkdownToc', () => {
  test('renders empty when no items', () => {
    const html = render(<MarkdownToc items={[]} />);
    expect(html).toBe('');
  });

  test('renders list of headings mapped to anchors', () => {
    const items = [
      { id: 'intro', level: 1, text: 'Introduction' },
      { id: 'usage', level: 2, text: 'Usage' },
    ];

    const html = render(<MarkdownToc items={items} />);

    expect(html).toContain('On this page');
    expect(html).toContain('href="#intro"');
    expect(html).toContain('Introduction');
    expect(html).toContain('href="#usage"');
    expect(html).toContain('Usage');
  });
});
