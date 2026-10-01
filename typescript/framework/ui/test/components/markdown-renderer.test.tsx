import { describe, expect, test } from 'bun:test';
import { MarkdownRenderer } from '../../src/components/markdown-renderer';
import { render } from './render';

describe('MarkdownRenderer', () => {
  test('renders trusted structural HTML', () => {
    const htmlInput = '<h3>Rendered Title</h3><p>Text</p>';

    const html = render(<MarkdownRenderer html={htmlInput} />);

    expect(html).toContain('<h3>Rendered Title</h3>');
    expect(html).toContain('<p>Text</p>');
  });

  test('preserves internal links as native anchors (no router dependency)', () => {
    const html = render(<MarkdownRenderer html='<a href="/docs/getting-started">Start</a>' />);

    expect(html).toContain('href="/docs/getting-started"');
    expect(html).toContain('Start');
  });

  test('preserves code-group and mermaid containers used by the docs pipeline', () => {
    const htmlInput =
      '<div class="code-group"><button class="code-group-tab" data-label="TypeScript" data-index="0">TS</button></div>' +
      '<div class="mermaid-diagram">graph TD; A--&gt;B;</div>';

    const html = render(<MarkdownRenderer html={htmlInput} />);

    expect(html).toContain('class="code-group"');
    expect(html).toContain('data-label="TypeScript"');
    expect(html).toContain('class="mermaid-diagram"');
  });

  describe('sanitization (XSS)', () => {
    test('strips <script> tags and their contents', () => {
      const html = render(<MarkdownRenderer html={'<p>safe</p><script>window.__pwned = true;</script>'} />);

      expect(html).toContain('<p>safe</p>');
      expect(html).not.toContain('<script');
      expect(html).not.toContain('window.__pwned');
    });

    test('strips inline event-handler attributes', () => {
      const html = render(<MarkdownRenderer html={'<img src="/logo.png" onerror="alert(1)" alt="logo" />'} />);

      expect(html).not.toContain('onerror');
      expect(html).not.toContain('alert(1)');
      // The safe parts of the element survive.
      expect(html).toContain('src="/logo.png"');
    });

    test('strips javascript: URLs from links', () => {
      const html = render(<MarkdownRenderer html={'<a href="javascript:alert(1)">click</a>'} />);

      expect(html).not.toContain('javascript:');
      // The anchor text is preserved even though the dangerous href is dropped.
      expect(html).toContain('click');
    });

    test('strips entity-encoded javascript: URLs', () => {
      const html = render(<MarkdownRenderer html={'<a href="&#106;avascript:alert(1)">x</a>'} />);

      expect(html).not.toContain('alert(1)');
      expect(html).not.toMatch(/javascript:/i);
    });

    test('removes disallowed iframe elements', () => {
      const html = render(<MarkdownRenderer html={'<iframe src="https://evil.example"></iframe><p>ok</p>'} />);

      expect(html).not.toContain('<iframe');
      expect(html).toContain('<p>ok</p>');
    });

    test('drops inline style attributes used for CSS-based injection', () => {
      const html = render(<MarkdownRenderer html={'<p style="background:url(javascript:alert(1))">hi</p>'} />);

      expect(html).not.toContain('style=');
      expect(html).not.toContain('javascript:');
      expect(html).toContain('hi');
    });
  });
});
