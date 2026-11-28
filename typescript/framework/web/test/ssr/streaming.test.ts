import React, { Suspense, use } from 'react';
import { describe, expect, it } from 'bun:test';
import {
  CacheMiddleware,
  clearCache,
  CSP_NONCE_CONTEXT_KEY,
  type HttpRequestContext,
  type HttpResponse,
} from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { buildHttpContext } from '../../../application/src/http/http-context.builder';
import { runInContext } from '../../../runtime/src/context/context.utils';
import { useDocumentMeta } from '../../src/client/document/document-context';
import { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';
import { crawlerPattern, DEFAULT_CRAWLER_USER_AGENTS, isCrawler } from '../../src/ssr/crawlers';
import RootHtml from '../../src/ssr/default.html';
import { pageRenderer } from '../../src/ssr/page.renderer';
import { createPageRenderer } from '../../src/ssr/route-registration.utils';

const BROWSER = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36';
const GOOGLEBOT = 'Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)';

/** A promise the test resolves, standing for the data a boundary waits on. */
function gate() {
  let open!: () => void;
  const promise = new Promise<string>((resolve) => {
    open = () => resolve('Late content');
  });
  return { promise, open };
}

/**
 * A page whose shell sets a title, a meta tag and an extracted style, and whose
 * Suspense boundary, once its gate opens, adds another title, meta tag and
 * extracted style. The extractor returns what was inserted since its last
 * call, as the Emotion one of @putnami/ui does.
 */
function streamingPageElement(data: Promise<string>) {
  const inserted: string[] = [];
  let returned = 0;
  const extractor = () => {
    const fresh = inserted.slice(returned);
    returned = inserted.length;
    return fresh.map((name) => ({
      dangerouslySetInnerHTML: { __html: `.${name}{color:red}` },
      'data-emotion': `css ${name}`,
    }));
  };

  function Late() {
    const text = use(data);
    const meta = useDocumentMeta();
    if (meta) {
      const helper = new SsrDocumentHelper(meta);
      helper.title = 'Late title';
      helper.addMeta({ property: 'og:title', content: 'Late title' });
    }
    inserted.push('late');
    return React.createElement('p', null, text);
  }

  function Shell() {
    const meta = useDocumentMeta();
    if (meta && !meta.styleExtractors?.includes(extractor)) {
      const helper = new SsrDocumentHelper(meta);
      helper.title = 'Shell title';
      helper.addMeta({ property: 'og:title', content: 'Shell title' });
      meta.styleExtractors ||= [];
      meta.styleExtractors.push(extractor);
    }
    inserted.push('shell');
    return React.createElement(
      'main',
      null,
      React.createElement('h1', null, 'Shell'),
      React.createElement(Suspense, { fallback: React.createElement('p', null, 'Loading') }, React.createElement(Late)),
    );
  }

  return React.createElement(Shell);
}

function streamingPage(data: Promise<string>) {
  const element = streamingPageElement(data);
  return pageRenderer(
    () => [{ path: '/', element } as never],
    () => RootHtml,
    () => 'hydrate.js',
    5000,
    undefined,
    undefined,
    undefined,
    (req) => isCrawler(req, crawlers),
  );
}

const crawlers = crawlerPattern(DEFAULT_CRAWLER_USER_AGENTS);

async function request(userAgent: string, data: Promise<string>) {
  const ctx = buildHttpContext<HttpRequestContext>({
    req: new Request('http://localhost/', { headers: { 'user-agent': userAgent } }),
  });
  Object.assign(ctx, { params: {}, statusCode: 0 });
  const handler = streamingPage(data);
  const response = (await runInContext(ctx as never, async () => await handler(ctx as never))) as HttpResponse;
  const nonce = (ctx as unknown as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY] as string;
  return { body: response.get().body as ReadableStream<Uint8Array>, nonce };
}

/** Read the body until it contains `marker`, or until it ends. */
async function readUntil(reader: ReadableStreamDefaultReader<Uint8Array>, marker: string, start = '') {
  const decoder = new TextDecoder();
  let text = start;
  while (!text.includes(marker)) {
    const { done, value } = await reader.read();
    if (done) break;
    text += decoder.decode(value, { stream: true });
  }
  return text;
}

describe('SSR streaming', () => {
  specTest(
    'sends the head and the shell to a browser before a pending Suspense boundary resolves',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-streaming',
      check: 'the-shell-is-sent-before-a-pending-boundary-resolves',
    },
    async () => {
      const { promise, open } = gate();
      const { body } = await request(BROWSER, promise);
      const reader = body.getReader();

      const early = await readUntil(reader, 'Loading');
      expect(early).toContain('<title>Shell title</title>');
      expect(early).toContain('<h1>Shell</h1>');
      expect(early).toContain('Loading');
      expect(early).not.toContain('Late content');

      open();
      const whole = await readUntil(reader, '</html>', early);
      expect(whole).toContain('Late content');
    },
  );

  specTest(
    'writes a style a late boundary inserted before the chunk that shows it',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-streaming',
      check: 'a-late-style-is-written-before-the-chunk-that-needs-it',
    },
    async () => {
      const { promise, open } = gate();
      const { body } = await request(BROWSER, promise);
      open();
      const html = await new Response(body).text();
      const [head, rest] = html.split('</head>');

      expect(head).toContain('data-emotion="css shell"');
      expect(head).not.toContain('css late');
      const style = rest.indexOf('<style data-emotion="css late">.late{color:red}</style>');
      expect(style).toBeGreaterThan(-1);
      expect(style).toBeLessThan(rest.indexOf('Late content'));
      // Each style is written once.
      expect(html.split('.shell{color:red}').length - 1).toBe(1);
      expect(html.split('.late{color:red}').length - 1).toBe(1);
    },
  );

  specTest(
    'applies the head tags a late boundary added with one nonced script after the root',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-streaming',
      check: 'a-late-head-tag-is-applied-after-the-root',
    },
    async () => {
      const { promise, open } = gate();
      const { body, nonce } = await request(BROWSER, promise);
      open();
      const html = await new Response(body).text();
      const head = html.split('</head>')[0];
      expect(head).toContain('<title>Shell title</title>');
      expect(head).not.toContain('Late title');

      const afterRoot = html.slice(html.lastIndexOf('</div>'));
      const patch = afterRoot.match(/<script nonce="([^"]+)">\(function\(p\)\{[^<]*<\/script>/);
      expect(patch).not.toBeNull();
      expect(patch?.[1]).toBe(nonce);
      expect(patch?.[0]).toContain('"t":"Late title"');
      expect(patch?.[0]).toContain('meta[property=\\"og:title\\"]');
      expect(patch?.[0]).toContain('content=\\"Late title\\"');
      // The patch is the only script between the root and the hydration bundle.
      expect(afterRoot.indexOf(patch?.[0] ?? '')).toBeLessThan(afterRoot.indexOf('src="/hydrate.js"'));
    },
  );

  specTest(
    'waits for every boundary before the head of a crawler request',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-streaming',
      check: 'a-crawler-request-gets-every-head-contribution-in-the-head',
    },
    async () => {
      const { promise, open } = gate();
      const { body } = await request(GOOGLEBOT, promise);
      setTimeout(open, 10);
      const html = await new Response(body).text();
      const [head, rest] = html.split('</head>');

      expect(head).toContain('<title>Late title</title>');
      expect(head).toContain('content="Late title"');
      expect(head).toContain('.late{color:red}');
      expect(rest).toContain('Late content');
      expect(rest).not.toContain('(function(p){');
      expect(rest).not.toContain('<style');
    },
  );

  specTest(
    'renders a cached page whole, so a crawler served from the cache still gets every head contribution',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-streaming',
      check: 'a-cached-page-waits-for-every-boundary',
    },
    async () => {
      await clearCache();
      const { promise, open } = gate();
      setTimeout(open, 10);
      const page = streamingPageElement(promise);
      const renderer = createPageRenderer(
        {
          config: {
            ssrTimeout: 5000,
            clientFetchTimeout: 30_000,
            crawlerUserAgents: DEFAULT_CRAWLER_USER_AGENTS,
          } as never,
          routes: () => [{ path: '/', element: page } as never],
          basename: () => undefined,
          hydrateScript: () => 'hydrate.js',
        },
        '/cached',
        { ttl: 60_000 },
      );
      const call = async (userAgent: string) => {
        const ctx = buildHttpContext<HttpRequestContext>({
          req: new Request('http://localhost/', { headers: { 'user-agent': userAgent } }),
        });
        Object.assign(ctx, { params: {}, statusCode: 0, queryParams: () => ({}) });
        const response = (await runInContext(ctx as never, async () => await renderer(ctx as never))) as HttpResponse;
        return response.get().text();
      };

      // A browser fills the cache; the crawler reads that same HTML.
      const browser = await call(BROWSER);
      const crawler = await call(GOOGLEBOT);
      expect(crawler).toBe(browser);
      const [head, rest] = crawler.split('</head>');
      expect(head).toContain('<title>Late title</title>');
      expect(head).toContain('.late{color:red}');
      expect(rest).not.toContain('(function(p){');
    },
  );

  it('renders whole a page that a shared HTTP cache stores, as a layout cache sets it', async () => {
    const { promise, open } = gate();
    setTimeout(open, 10);
    const handler = streamingPage(promise);
    // The middleware a layout().cache({ maxAge, sMaxAge }) adds around the page.
    const layoutCache = CacheMiddleware({ maxAge: 300, sMaxAge: 600 });
    const ctx = buildHttpContext<HttpRequestContext>({
      req: new Request('http://localhost/', { headers: { 'user-agent': BROWSER } }),
    });
    Object.assign(ctx, { params: {}, statusCode: 0 });
    const response = (await runInContext(ctx as never, async () =>
      layoutCache(ctx as never, async () => (await handler(ctx as never)) as HttpResponse),
    )) as HttpResponse;
    const html = await response.get().text();
    const [head, rest] = html.split('</head>');
    expect(head).toContain('<title>Late title</title>');
    expect(rest).not.toContain('(function(p){');
  });

  it('writes a script added again after the shell once', async () => {
    const meta = {};
    const helper = new SsrDocumentHelper(meta, 'n');
    helper.addScript({ type: 'application/ld+json', children: '{"a":1}' });
    expect(helper.headHtml).toContain('{"a":1}');
    // The same script again, as a late boundary would add it: a new object, the same HTML.
    helper.addScript({ type: 'application/ld+json', children: '{"a":1}' });
    expect(helper.lateHeadHtml()).toBe('');
  });

  it('names search crawlers and link-preview agents, and not browsers', () => {
    const req = (agent: string) => new Request('http://localhost/', { headers: { 'user-agent': agent } });
    for (const agent of [
      GOOGLEBOT,
      'Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)',
      'facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)',
      'Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)',
      'Twitterbot/1.0',
      'LinkedInBot/1.0 (compatible; Mozilla/5.0)',
      'WhatsApp/2.23.20.0',
      'Mozilla/5.0 (compatible; Bluesky Cardyb/1.1; +mailto:support@bsky.app)',
      'Iframely/1.3.1 (+https://iframely.com/docs/about)',
    ]) {
      expect(isCrawler(req(agent), crawlers)).toBe(true);
    }
    for (const agent of [
      BROWSER,
      // Lighthouse audits the page a browser gets.
      'Mozilla/5.0 (Linux; Android 11; moto g power (2022)) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Mobile Safari/537.36 Chrome-Lighthouse',
      // A phone brand, and in-app browsers of chat applications.
      'Mozilla/5.0 (Linux; Android 13; CUBOT KINGKONG 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Mobile Safari/537.36',
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) discord/1.0.9 Chrome/134.0 Electron/35.0 Safari/537.36',
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Slack/4.41.105 Chrome/134.0 Electron/35.0 Safari/537.36',
    ]) {
      expect(isCrawler(req(agent), crawlers)).toBe(false);
    }
    expect(isCrawler(new Request('http://localhost/'), crawlers)).toBe(false);
    expect(crawlerPattern('')).toBeUndefined();
    expect(isCrawler(req(GOOGLEBOT), crawlerPattern(''))).toBe(false);
  });
});
