import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { escapeHtml } from '../../src/client/document/tag.utils';
import { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';
import { parseIslandProps, serializeIslandProps } from '../../src/client/island/island-types';

describe('XSS escaping', () => {
  describe('escapeHtml', () => {
    it('escapes ampersands', () => {
      expect(escapeHtml('a&b')).toBe('a&amp;b');
    });

    it('escapes less-than', () => {
      expect(escapeHtml('a<b')).toBe('a&lt;b');
    });

    it('escapes greater-than', () => {
      expect(escapeHtml('a>b')).toBe('a&gt;b');
    });

    it('escapes double quotes', () => {
      expect(escapeHtml('a"b')).toBe('a&quot;b');
    });

    it('escapes single quotes', () => {
      expect(escapeHtml("a'b")).toBe('a&#39;b');
    });

    it('escapes script injection in title', () => {
      const malicious = '</title><script>alert(1)</script>';
      const escaped = escapeHtml(malicious);
      expect(escaped).not.toContain('</title>');
      expect(escaped).not.toContain('<script>');
      expect(escaped).toContain('&lt;/title&gt;');
    });
  });

  describe('SsrDocumentHelper title escaping', () => {
    it('escapes HTML in title within headHtml', () => {
      const helper = new SsrDocumentHelper({});
      helper.title = '<script>alert("xss")</script>';

      const headHtml = helper.headHtml;
      expect(headHtml).not.toContain('<script>alert');
      expect(headHtml).toContain('&lt;script&gt;');
    });

    it('escapes closing title tag injection', () => {
      const helper = new SsrDocumentHelper({});
      helper.title = '</title><img src=x onerror=alert(1)>';

      const headHtml = helper.headHtml;
      expect(headHtml).not.toContain('</title><img');
      expect(headHtml).toContain('&lt;/title&gt;');
    });
  });

  describe('SsrDocumentHelper lang escaping', () => {
    // page.renderer.ts interpolates escapeHtml(docHelper.lang) into
    // `<html lang="...">`, so a hostile lang must not break out of the attribute.
    it('escapes a malicious lang value for the <html lang> attribute', () => {
      const helper = new SsrDocumentHelper({});
      helper.lang = '"><script>alert(1)</script>';

      const attr = escapeHtml(helper.lang);
      expect(attr).not.toContain('"');
      expect(attr).not.toContain('<script>');
      expect(attr).toContain('&quot;');
      expect(attr).toContain('&lt;script&gt;');
    });

    it('defaults to "en" when lang is unset', () => {
      const helper = new SsrDocumentHelper({});
      expect(helper.lang).toBe('en');
    });
  });

  describe('serializeIslandProps', () => {
    // Island props are embedded verbatim inside a <script> hydration payload, so
    // the same break-out sequences that threaten safeJsonForScript apply here.
    specTest(
      'escapes </script> break-out in island props',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'hydration-containment',
        check: 'a-script-break-out-in-serialized-state-is-escaped',
      },
      () => {
        const props = { html: '</script><script>alert(1)</script>' };
        const result = serializeIslandProps(props);
        expect(result).not.toContain('</script>');
        expect(result).not.toContain('<script>');
        expect(JSON.parse(result)).toEqual(props);
      },
    );

    specTest(
      'escapes the U+2028 / U+2029 line separators',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'hydration-containment',
        check: 'the-unicode-line-separators-are-escaped',
      },
      () => {
        const ls = String.fromCharCode(0x20_28);
        const ps = String.fromCharCode(0x20_29);
        const props = { text: `line${ls}sep${ps}end` };
        const result = serializeIslandProps(props);
        // Raw separators would truncate the inline script; they must be escaped.
        expect(result).not.toContain(ls);
        expect(result).not.toContain(ps);
        expect(result).toContain('\\u2028');
        expect(result).toContain('\\u2029');
        expect(JSON.parse(result)).toEqual(props);
      },
    );

    it('round-trips a hostile payload through parseIslandProps unchanged', () => {
      const ls = String.fromCharCode(0x20_28);
      const props = { evil: `</script>${ls}<img src=x onerror=alert(1)>` };
      const serialized = serializeIslandProps(props);
      expect(serialized).not.toContain('</script>');
      expect(serialized).not.toContain(ls);
      // The escaped blob decodes back to the original object.
      expect(parseIslandProps(serialized)).toEqual(props);
    });

    it('serializes nullish props as an empty object', () => {
      expect(serializeIslandProps(undefined)).toBe('{}');
      expect(serializeIslandProps(null)).toBe('{}');
    });
  });
});
