import { Desc, HttpResponse, endpoint } from '@putnami/application';
import { joinPath } from '@putnami/utils';
import { getDocsRoot, resolveContentPath } from '../../lib/docs/navigation.server';

const QUERY = {
  path: Desc('Docs URL path to fetch as raw Markdown, e.g. /docs/frameworks/typescript/overview.', String),
};

/** Normalize a `?path=` value (with or without a leading /docs/) to a docs URL path. */
function toDocsUrlPath(raw: string): string {
  let urlPath = decodeURIComponent(raw).trim().replace(/^\/+/, '').replace(/\/+$/, '');
  if (urlPath === 'docs') return '';
  if (urlPath.startsWith('docs/')) urlPath = urlPath.slice('docs/'.length);
  return urlPath;
}

// Serves a documentation page as raw source Markdown — the LLM/agent-friendly
// view behind the docs' "Copy as Markdown" affordance. A dedicated endpoint
// (rather than a `/docs/*.md` route) keeps it off the React catch-all while
// resolving against the same nav tree the pages and sitemap use.
export default endpoint()
  .description('Serve a documentation page as raw source Markdown for LLMs and agents.')
  .query(QUERY)
  .cache({ maxAge: 3600, sMaxAge: 3600, staleWhileRevalidate: 3600 })
  .handle(async (ctx) => {
    const urlPath = toDocsUrlPath(ctx.queryParams().path ?? '');
    const contentPath = urlPath ? await resolveContentPath(urlPath) : undefined;

    if (!contentPath) {
      return new HttpResponse('Not found', {
        status: 404,
        headers: { 'Content-Type': 'text/plain; charset=utf-8' },
      });
    }

    const content = await Bun.file(joinPath(getDocsRoot(), contentPath)).text();
    return new HttpResponse(content, {
      headers: {
        'Content-Type': 'text/markdown; charset=utf-8',
        'Cache-Control': 'public, max-age=3600',
      },
    });
  });
