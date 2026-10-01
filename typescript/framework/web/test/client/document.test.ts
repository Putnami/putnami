import { beforeEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { documentHelper } from '../../src/client/document/document.helper';
import { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';

describe('Document Helpers', () => {
  describe('documentHelper factory', () => {
    it('returns SsrDocumentHelper in Node.js environment (no window)', () => {
      // In Bun test environment, window is undefined, so we get SSR helper
      const helper = documentHelper();

      expect(helper).toBeInstanceOf(SsrDocumentHelper);
    });
  });

  describe('SsrDocumentHelper', () => {
    let helper: SsrDocumentHelper;

    beforeEach(() => {
      helper = new SsrDocumentHelper();
    });

    describe('title', () => {
      it('has default empty title', () => {
        expect(helper.title).toBe('');
      });

      it('can set and get title', () => {
        helper.title = 'Test Page';
        expect(helper.title).toBe('Test Page');
      });
    });

    describe('lang', () => {
      it('has default lang of "en"', () => {
        expect(helper.lang).toBe('en');
      });

      it('can set and get lang', () => {
        helper.lang = 'fr';
        expect(helper.lang).toBe('fr');
      });
    });

    describe('CSP nonce', () => {
      specTest(
        'stamps the per-request nonce on head-injected scripts',
        {
          feature: 'typescript/web-application-delivery',
          requirement: 'csrf-and-csp',
          check: 'a-head-injected-script-carries-the-per-request-nonce',
        },
        () => {
          const nonced = new SsrDocumentHelper({}, 'abc123');
          nonced.addScript({ type: 'text/javascript', children: 'window.__colorMode="dark";' });
          nonced.addScript({ src: '/app.js', type: 'module' });

          const headHtml = nonced.headHtml;
          const matches = headHtml.match(/nonce="abc123"/g);
          expect(matches).toHaveLength(2);
        },
      );

      it('keeps an explicitly set script nonce', () => {
        const nonced = new SsrDocumentHelper({}, 'abc123');
        nonced.addScript({ src: '/app.js', nonce: 'explicit' });

        expect(nonced.headHtml).toContain('nonce="explicit"');
        expect(nonced.headHtml).not.toContain('nonce="abc123"');
      });

      it('emits no nonce attribute when none is provided', () => {
        helper.addScript({ src: '/app.js' });
        expect(helper.headHtml).not.toContain('nonce=');
      });
    });

    describe('addScript', () => {
      it('adds script to the document', () => {
        helper.addScript({ src: '/app.js', type: 'module' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('<script');
        expect(headHtml).toContain('src="/app.js"');
        expect(headHtml).toContain('type="module"');
      });

      it('adds multiple scripts', () => {
        helper.addScript({ src: '/vendor.js' });
        helper.addScript({ src: '/app.js' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('vendor.js');
        expect(headHtml).toContain('app.js');
      });

      specTest(
        'deduplicates scripts by src',
        {
          feature: 'typescript/web-application-delivery',
          requirement: 'document-isolation',
          check: 'a-script-is-de-duplicated-by-src',
        },
        () => {
          helper.addScript({ src: '/app.js', type: 'module' });
          helper.addScript({ src: '/app.js', type: 'module', crossOrigin: 'anonymous' });

          const headHtml = helper.headHtml;
          const matches = headHtml.match(/src="\/app\.js"/g);
          expect(matches).toHaveLength(1);
          expect(headHtml).toContain('crossorigin="anonymous"');
        },
      );

      specTest(
        'deduplicates inline scripts by type',
        {
          feature: 'typescript/web-application-delivery',
          requirement: 'document-isolation',
          check: 'an-inline-script-is-de-duplicated-by-type',
        },
        () => {
          const jsonLd1 = JSON.stringify({ '@type': 'WebSite', name: 'Old' });
          const jsonLd2 = JSON.stringify({ '@type': 'WebSite', name: 'New' });
          helper.addScript({ type: 'application/ld+json', children: jsonLd1 });
          helper.addScript({ type: 'application/ld+json', children: jsonLd2 });

          const headHtml = helper.headHtml;
          const matches = headHtml.match(/application\/ld\+json/g);
          expect(matches).toHaveLength(1);
          expect(headHtml).not.toContain('"name":"Old"');
          expect(headHtml).toContain('"name":"New"');
        },
      );

      it('does not escape inline script content (raw text element)', () => {
        const jsonLd = JSON.stringify({ '@type': 'WebSite', name: 'Test & "Quotes"' });
        helper.addScript({ type: 'application/ld+json', children: jsonLd });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('application/ld+json');
        // JSON content must not be HTML-escaped inside <script> tags
        expect(headHtml).toContain('"@type":"WebSite"');
        expect(headHtml).toContain('"Test & \\"Quotes\\""');
        expect(headHtml).not.toContain('&amp;');
      });
    });

    describe('addLink', () => {
      it('adds link to the document', () => {
        helper.addLink({ rel: 'stylesheet', href: '/styles.css' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('<link');
        expect(headHtml).toContain('rel="stylesheet"');
        expect(headHtml).toContain('href="/styles.css"');
      });

      it('adds favicon link', () => {
        helper.addLink({ rel: 'icon', href: '/favicon.ico' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('rel="icon"');
        expect(headHtml).toContain('href="/favicon.ico"');
      });
    });

    describe('addMeta', () => {
      it('adds meta tag to the document', () => {
        helper.addMeta({ name: 'description', content: 'Test description' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('<meta');
        expect(headHtml).toContain('name="description"');
        expect(headHtml).toContain('content="Test description"');
      });

      it('adds charset meta', () => {
        helper.addMeta({ charSet: 'utf-8' } as any);

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('<meta');
      });

      it('adds viewport meta', () => {
        helper.addMeta({ name: 'viewport', content: 'width=device-width, initial-scale=1' });

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('viewport');
      });
    });

    describe('addStyle', () => {
      it('adds inline style to the document', () => {
        helper.addStyle({ children: 'body { margin: 0; }' } as any);

        const headHtml = helper.headHtml;
        expect(headHtml).toContain('<style');
      });
    });

    describe('headHtml', () => {
      it('returns complete head HTML', () => {
        helper.title = 'My App';
        helper.addMeta({ name: 'description', content: 'My Description' });
        helper.addLink({ rel: 'stylesheet', href: '/style.css' });

        const headHtml = helper.headHtml;

        expect(headHtml).toContain('<head>');
        expect(headHtml).toContain('<title>My App</title>');
        expect(headHtml).toContain('</head>');
      });

      it('generates empty head when no content set', () => {
        const headHtml = helper.headHtml;

        expect(headHtml).toContain('<head>');
        expect(headHtml).toContain('</head>');
      });
    });
  });
});
