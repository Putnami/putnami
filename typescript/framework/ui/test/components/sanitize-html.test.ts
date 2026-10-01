import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { sanitizeHtml } from '../../src/components/sanitize-html';

describe('sanitizeHtml', () => {
  describe('preserves safe markup', () => {
    specTest(
      'keeps structural and semantic tags',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'structural-and-semantic-markup-is-preserved',
      },
      () => {
        const input = '<h1>Title</h1><p>A <strong>bold</strong> and <em>italic</em> word.</p>';
        expect(sanitizeHtml(input)).toBe(input);
      },
    );

    it('keeps lists and tables', () => {
      const input =
        '<ul><li>one</li><li>two</li></ul>' +
        '<table><thead><tr><th>H</th></tr></thead><tbody><tr><td>D</td></tr></tbody></table>';
      expect(sanitizeHtml(input)).toBe(input);
    });

    specTest(
      'keeps code blocks with classes',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'code-blocks-keep-their-classes',
      },
      () => {
        const input = '<pre class="shiki"><code class="language-ts">const x = 1;</code></pre>';
        expect(sanitizeHtml(input)).toBe(input);
      },
    );

    it('keeps safe Shiki color variables for highlighted code', () => {
      const input =
        '<pre class="shiki" style="--shiki-light:#24292e;--shiki-dark:#e1e4e8;--shiki-light-bg:#fff;--shiki-dark-bg:#24292e">' +
        '<code><span style="--shiki-light:#032F62;--shiki-dark:#9ECBFF">putnami</span></code></pre>';

      expect(sanitizeHtml(input)).toBe(input);
    });

    specTest(
      'keeps data-* and aria-* attributes (code groups, a11y)',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'data-and-aria-attributes-are-preserved',
      },
      () => {
        const input = '<button class="code-group-tab" data-label="Go" data-index="1" aria-selected="true">Go</button>';
        const out = sanitizeHtml('<div data-label="Go" data-index="1" aria-hidden="false">x</div>');
        expect(out).toContain('data-label="Go"');
        expect(out).toContain('data-index="1"');
        expect(out).toContain('aria-hidden="false"');
        // Code-group tabs are part of the markdown docs pipeline and should survive.
        expect(sanitizeHtml(input)).toBe(input);
      },
    );

    it('keeps internal, anchor, and query relative hrefs', () => {
      expect(sanitizeHtml('<a href="/docs">x</a>')).toContain('href="/docs"');
      expect(sanitizeHtml('<a href="#section">x</a>')).toContain('href="#section"');
      expect(sanitizeHtml('<a href="?q=1">x</a>')).toContain('href="?q=1"');
      expect(sanitizeHtml('<a href="https://putnami.dev">x</a>')).toContain('href="https://putnami.dev"');
      expect(sanitizeHtml('<a href="mailto:a@b.com">x</a>')).toContain('href="mailto:a@b.com"');
    });

    it('keeps data:image sources but not other data URIs', () => {
      const img = '<img src="data:image/png;base64,iVBORw0KGgo=" alt="x" />';
      expect(sanitizeHtml(img)).toContain('src="data:image/png;base64,iVBORw0KGgo="');

      const bad = '<img src="data:text/html;base64,PHNjcmlwdD4=" alt="x" />';
      expect(sanitizeHtml(bad)).not.toContain('data:text/html');
    });
  });

  describe('removes dangerous markup', () => {
    specTest(
      'removes <script> and its contents',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'script-elements-are-removed-with-their-contents',
      },
      () => {
        expect(sanitizeHtml('<script>evil()</script><p>ok</p>')).toBe('<p>ok</p>');
      },
    );

    specTest(
      'removes <style> and its contents',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'style-elements-are-removed-with-their-contents',
      },
      () => {
        expect(sanitizeHtml('<style>body{display:none}</style><p>ok</p>')).toBe('<p>ok</p>');
      },
    );

    specTest(
      'removes <iframe>, <object>, and <embed>',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'iframe-object-and-embed-are-removed',
      },
      () => {
        expect(sanitizeHtml('<iframe src="https://evil"></iframe>')).toBe('');
        expect(sanitizeHtml('<object data="x.swf"></object>')).toBe('');
        expect(sanitizeHtml('<embed src="x.swf" />')).toBe('');
      },
    );

    specTest(
      'strips on* event-handler attributes',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'event-handler-attributes-are-stripped',
      },
      () => {
        const out = sanitizeHtml('<img src="/a.png" onerror="alert(1)" onload="x()" alt="a" />');
        expect(out).not.toContain('onerror');
        expect(out).not.toContain('onload');
        expect(out).not.toContain('alert(1)');
        expect(out).toContain('src="/a.png"');
        expect(out).toContain('alt="a"');
      },
    );

    specTest(
      'strips inline style attributes',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'inline-style-attributes-are-stripped',
      },
      () => {
        const out = sanitizeHtml('<p style="color:red">x</p>');
        expect(out).toBe('<p>x</p>');
      },
    );

    it('keeps a percentage width on <col> (markdown column weights)', () => {
      const out = sanitizeHtml(
        '<table><colgroup><col style="width:24.0%"/><col style="width:76%"/></colgroup></table>',
      );
      expect(out).toContain('<col style="width:24.0%" />');
      expect(out).toContain('<col style="width:76%" />');
    });

    it('strips every other style from <col>', () => {
      const out = sanitizeHtml(
        '<table><colgroup>' +
          '<col style="width:24%;background:url(javascript:alert(1))"/>' +
          '<col style="background:red"/>' +
          '<col style="width:24px"/>' +
          '<col style="width:9999%"/>' +
          '</colgroup></table>',
      );
      expect(out).not.toContain('style');
    });

    it('strips non-Shiki styles from code blocks', () => {
      const out = sanitizeHtml(
        '<pre class="shiki" style="background:url(javascript:alert(1));--shiki-dark:#e1e4e8">' +
          '<code><span style="color:black;--shiki-light:#032F62">x</span></code></pre>',
      );

      expect(out).not.toContain('background');
      expect(out).not.toContain('javascript');
      expect(out).not.toContain('color:black');
      expect(out).toContain('style="--shiki-dark:#e1e4e8"');
      expect(out).toContain('style="--shiki-light:#032F62"');
    });

    specTest(
      'drops javascript: and vbscript: hrefs but keeps the element text',
      {
        feature: 'typescript/ui-system',
        requirement: 'external-link-containment',
        check: 'a-rejected-url-loses-its-target-but-keeps-its-text',
      },
      () => {
        expect(sanitizeHtml('<a href="javascript:alert(1)">link</a>')).toBe('<a>link</a>');
        expect(sanitizeHtml('<a href="vbscript:msgbox(1)">link</a>')).toBe('<a>link</a>');
      },
    );

    specTest(
      'defeats entity-encoded scheme obfuscation',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'entity-encoded-scheme-obfuscation-is-defeated',
      },
      () => {
        const out = sanitizeHtml('<a href="&#106;avascript:alert(1)">x</a>');
        expect(out).not.toMatch(/alert\(1\)/);
        expect(out).toBe('<a>x</a>');
      },
    );

    specTest(
      'defeats whitespace/control-char obfuscation in schemes',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'whitespace-obfuscated-schemes-are-defeated',
      },
      () => {
        const out = sanitizeHtml('<a href="java\tscript:alert(1)">x</a>');
        expect(out).toBe('<a>x</a>');
      },
    );

    specTest(
      'removes HTML comments (which can hide conditional scripts)',
      { feature: 'typescript/ui-system', requirement: 'rich-content-sanitization', check: 'html-comments-are-removed' },
      () => {
        expect(sanitizeHtml('<!-- <script>x()</script> --><p>ok</p>')).toBe('<p>ok</p>');
      },
    );

    it('unwraps disallowed tags but preserves their text content', () => {
      // <marquee> is not allowlisted; its text survives, the tag does not.
      expect(sanitizeHtml('<marquee>hello</marquee>')).toBe('hello');
    });

    specTest(
      'adds rel=noopener to target=_blank links',
      {
        feature: 'typescript/ui-system',
        requirement: 'external-link-containment',
        check: 'a-new-context-link-receives-noopener',
      },
      () => {
        const out = sanitizeHtml('<a href="https://x.example" target="_blank">x</a>');
        expect(out).toContain('rel="noopener noreferrer"');
      },
    );
  });

  describe('attribute allowlist', () => {
    specTest(
      'drops attributes no tag allowlist names, not only the explicitly denied ones',
      {
        feature: 'typescript/ui-system',
        requirement: 'rich-content-sanitization',
        check: 'only-allowlisted-attributes-survive',
      },
      () => {
        // The requirement names an ALLOWLIST as the mechanism. The explicit
        // denials above it (on*, style, srcdoc, formaction, xlink:href) are
        // belt-and-braces: each is redundant with the allowlist, so a test that
        // only exercises them passes even with the allowlist opened.
        //
        // These attributes are denied by the allowlist ALONE — none is on a
        // deny line — and two of them are live vectors in sanitized user
        // content: `ping` fires a background request on click, and `background`
        // loads a remote image. Neither is scheme-checked, because neither is a
        // URL_ATTRS member.
        for (const [input, banned] of [
          ['<a href="/x" ping="https://tracker.example/beacon">t</a>', 'ping'],
          ['<td background="https://tracker.example/b.png">c</td>', 'background'],
          ['<div contenteditable="true">x</div>', 'contenteditable'],
          ['<a href="/x" download="payload.exe">d</a>', 'download'],
        ] as const) {
          const out = sanitizeHtml(input);
          expect(out).not.toContain(banned);
        }

        // The allowlist still admits what it documents, so the assertion above
        // cannot be satisfied by stripping everything.
        expect(sanitizeHtml('<a href="/x" title="t" rel="nofollow">k</a>')).toContain('rel="nofollow"');
        expect(sanitizeHtml('<td colspan="2">c</td>')).toContain('colspan="2"');
      },
    );
  });

  describe('edge cases', () => {
    it('returns empty string for empty input', () => {
      expect(sanitizeHtml('')).toBe('');
    });

    it('handles plain text without tags', () => {
      expect(sanitizeHtml('just text & more')).toBe('just text & more');
    });

    it('does not execute or leak content of an unterminated script tag', () => {
      // No closing tag: everything after the opener is suppressed.
      expect(sanitizeHtml('<p>before</p><script>evil()')).toBe('<p>before</p>');
    });
  });
});
