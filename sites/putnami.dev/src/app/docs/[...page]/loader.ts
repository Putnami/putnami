import { loader } from '@putnami/web';
import type { TocItem } from '@putnami/ui';
import { fileExists, joinPath } from '@putnami/utils';
import type { Tokens } from 'marked';
import { marked } from 'marked';
import { getDocsRoot, resolveContentPath } from '../../../lib/docs/navigation.server';
import { extractToc } from '../../../lib/docs-loader';
import { renderMarkdown } from '../../../lib/markdown';
import { MARKED_OPTIONS, toPlainText } from '../../../lib/markdown-utils';

interface PrerenderedDoc {
  html: string;
  toc: TocItem[];
}

async function loadPrerendered(filePath: string): Promise<PrerenderedDoc | undefined> {
  const cachePath = `${filePath}.rendered.json`;
  if (!fileExists(cachePath)) return undefined;
  return JSON.parse(await Bun.file(cachePath).text()) as PrerenderedDoc;
}

export interface DocLoaderData {
  html: string;
  toc: TocItem[];
  path: string[];
  title: string;
  description: string;
}

function resolveLegacyDocsRedirect(urlPath: string): string | undefined {
  if (urlPath === 'framework') {
    return '/docs/frameworks/typescript';
  }

  if (!urlPath.startsWith('framework/')) {
    return undefined;
  }

  const legacySectionPath = urlPath.slice('framework/'.length);
  if (legacySectionPath === '' || legacySectionPath === 'index') {
    return '/docs/frameworks/typescript';
  }

  if (legacySectionPath === 'go-framework') {
    // Go framework docs are published under /docs/frameworks/go — a redirect to
    // /docs/go/frameworks/... would trade one 404 for another.
    return '/docs/frameworks/go';
  }

  return `/docs/frameworks/typescript/${legacySectionPath}`;
}

// Runs at build time and bakes its result into the pre-rendered HTML. It only
// reads params['*'] + the generated docs filesystem (no request data), so it is
// static-safe; on the cloud runtime it also re-runs for ISR refreshes.
export default loader()
  .cache({ maxAge: 60 * 60 * 24, etag: true, ttl: 60 * 60 * 24 * 30 })
  .static()
  .handle(async ({ params }): Promise<DocLoaderData> => {
    // Catch-all routes provide the remaining path as '*' param
    let splatPath = params?.['*'] as string | undefined;
    if (!splatPath) {
      throw new Response('Path is required', { status: 400 });
    }

    // Decode URL-encoded characters (e.g. %26 → &) since the HTTP router
    // provides the raw URL path without decoding.
    splatPath = decodeURIComponent(splatPath);

    // Strip .json suffix (added by client-side loader fetch)
    const isLoaderJsonRequest = splatPath.endsWith('.json');
    if (isLoaderJsonRequest) {
      splatPath = splatPath.slice(0, -5);
    }

    const pathSegments = splatPath.split('/').filter(Boolean);
    const urlPath = splatPath;
    const legacyRedirectPath = resolveLegacyDocsRedirect(urlPath);
    if (legacyRedirectPath) {
      const location = isLoaderJsonRequest ? `${legacyRedirectPath}.json` : legacyRedirectPath;
      throw Response.redirect(location, 301);
    }

    const contentPath = await resolveContentPath(urlPath);

    if (!contentPath) {
      throw new Response('Document not found', { status: 404 });
    }

    const filePath = joinPath(getDocsRoot(), contentPath);
    const content = await Bun.file(filePath).text();
    const prerendered = await loadPrerendered(filePath);
    const [html, toc] = prerendered
      ? [prerendered.html, prerendered.toc]
      : await Promise.all([renderMarkdown(content), extractToc(content)]);

    // Extract title from first h1 heading
    const title =
      toc.find((item) => item.level === 1)?.text || pathSegments[pathSegments.length - 1] || 'Documentation';

    // Extract description from first paragraph
    const tokens = marked.lexer(content, MARKED_OPTIONS);
    const firstParagraph = tokens.find((t): t is Tokens.Paragraph => t.type === 'paragraph');
    const description = firstParagraph ? toPlainText(firstParagraph.raw).slice(0, 160) : `Documentation for ${title}`;

    return { html, toc, path: pathSegments, title, description };
  });
