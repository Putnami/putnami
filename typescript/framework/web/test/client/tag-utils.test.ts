import { describe, expect, it } from 'bun:test';
import { asHtml, escapeCssAttrValue, escapeHtml, escapeRawText } from '../../src/client/document/tag.utils';

describe('escapeHtml', () => {
  it('escapes ampersands', () => {
    expect(escapeHtml('a&b')).toBe('a&amp;b');
  });

  it('escapes angle brackets', () => {
    expect(escapeHtml('<script>alert("xss")</script>')).toBe('&lt;script&gt;alert(&quot;xss&quot;)&lt;/script&gt;');
  });

  it('escapes quotes', () => {
    expect(escapeHtml('"hello"')).toBe('&quot;hello&quot;');
    expect(escapeHtml("'world'")).toBe('&#39;world&#39;');
  });

  it('handles empty string', () => {
    expect(escapeHtml('')).toBe('');
  });

  it('passes through safe text unchanged', () => {
    expect(escapeHtml('hello world')).toBe('hello world');
  });
});

describe('escapeCssAttrValue', () => {
  it('escapes backslashes', () => {
    expect(escapeCssAttrValue('a\\b')).toBe('a\\\\b');
  });

  it('escapes double quotes', () => {
    expect(escapeCssAttrValue('a"b')).toBe('a\\"b');
  });

  it('handles combined escapes', () => {
    expect(escapeCssAttrValue('a\\b"c')).toBe('a\\\\b\\"c');
  });

  it('handles empty string', () => {
    expect(escapeCssAttrValue('')).toBe('');
  });
});

describe('asHtml', () => {
  it('renders a self-closing tag with no content', () => {
    const html = asHtml('meta', { name: 'viewport', content: 'width=device-width' });
    expect(html).toContain('<meta ');
    expect(html).toContain('name="viewport"');
    expect(html).toContain('content="width=device-width"');
    expect(html).toContain('/>');
  });

  it('renders a tag with text children', () => {
    const html = asHtml('title', { children: 'Hello World' });
    expect(html).toContain('>');
    expect(html).toContain('Hello World');
    expect(html).toContain('</title>');
  });

  it('escapes children content for non-raw-text tags', () => {
    const html = asHtml('title', { children: 'a & b < c' });
    expect(html).toContain('a &amp; b &lt; c');
  });

  it('does not escape content inside script tags', () => {
    const html = asHtml('script', { children: 'var x = 1 < 2;' });
    expect(html).toContain('var x = 1 < 2;');
    expect(html).not.toContain('&lt;');
  });

  it('does not escape content inside style tags', () => {
    const html = asHtml('style', { children: 'body { color: "red"; }' });
    expect(html).toContain('body { color: "red"; }');
  });

  it('supports dangerouslySetInnerHTML', () => {
    const html = asHtml('div', { dangerouslySetInnerHTML: { __html: '<b>bold</b>' } } as never);
    expect(html).toContain('<b>bold</b>');
    expect(html).toContain('</div>');
  });

  it('skips children and dangerouslySetInnerHTML from attributes', () => {
    const html = asHtml('div', { children: 'text', id: 'test' });
    expect(html).not.toContain('children=');
    expect(html).toContain('id="test"');
  });

  it('escapes attribute values', () => {
    const html = asHtml('meta', { name: 'desc', content: 'a "quoted" & <tag>' });
    expect(html).toContain('content="a &quot;quoted&quot; &amp; &lt;tag&gt;"');
  });

  it('skips null and undefined attribute values', () => {
    const html = asHtml('meta', { name: 'test', content: undefined as never });
    expect(html).toContain('name="test"');
    expect(html).not.toContain('content=');
  });

  it('neutralizes </script> breakout in script children', () => {
    const html = asHtml('script', { children: 'var a = "</script><script>alert(1)</script>";' });
    // The injected closing tag must not appear verbatim, so it cannot terminate the element.
    expect(html).not.toContain('</script><script>');
    expect(html).toContain('<\\/script>');
    // Only the final, framework-emitted closing tag remains.
    expect(html.match(/<\/script>/g)).toHaveLength(1);
  });

  it('neutralizes </style> breakout in style children', () => {
    const html = asHtml('style', { children: 'a{}</style><script>alert(1)</script>' });
    expect(html).not.toContain('</style><script>');
    expect(html).toContain('<\\/style>');
  });

  it('neutralizes </script> breakout in script dangerouslySetInnerHTML', () => {
    const html = asHtml('script', {
      dangerouslySetInnerHTML: { __html: 'x=1;</script><img src=x onerror=alert(1)>' },
    } as never);
    expect(html).not.toContain('</script><img');
    expect(html).toContain('<\\/script>');
  });
});

describe('escapeRawText', () => {
  it('splits closing script tags case-insensitively', () => {
    expect(escapeRawText('</script>')).toBe('<\\/script>');
    expect(escapeRawText('</SCRIPT >')).toBe('<\\/SCRIPT >');
  });

  it('splits closing style tags case-insensitively', () => {
    expect(escapeRawText('</style>')).toBe('<\\/style>');
  });

  it('splits HTML comment openers', () => {
    expect(escapeRawText('<!-- hi -->')).toBe('<\\!-- hi -->');
  });

  it('leaves benign comparison operators untouched', () => {
    expect(escapeRawText('if (a < b && c > d) {}')).toBe('if (a < b && c > d) {}');
  });

  it('leaves opening script-like substrings untouched (only end tags break out)', () => {
    expect(escapeRawText('el.querySelector("script")')).toBe('el.querySelector("script")');
  });
});
