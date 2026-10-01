import { HttpResponse, endpoint } from '@putnami/application';
import { type NavItem, toUrlPath } from '../../lib/docs/navigation';
import { getNavTree } from '../../lib/docs/navigation.server';

const BASE_URL = 'https://putnami.dev';

const SUMMARY =
  'Branch-native, local-first monorepo tooling. One workspace structure from laptop to preview to production, ' +
  'documented as one site across Tooling, TypeScript, Go, the managed Platform surface, and Python. ' +
  'Python is experimental, requires explicit opt-in, is never enabled by default, and carries no Go or TypeScript parity promise.';

/** Flatten a nav subtree into `[title](absolute-url)` markdown bullets. */
function collectLinks(items: NavItem[], lines: string[]): void {
  for (const item of items) {
    const path = toUrlPath(item.contentPath);
    if (path !== '#') {
      lines.push(`- [${item.name}](${BASE_URL}${path})`);
    }
    if (item.children?.length) {
      collectLinks(item.children, lines);
    }
  }
}

// Serves /llms.txt — a machine-readable index of the docs for LLMs and agents,
// built from the same nav tree as the sitemap and the SSG pre-render so it can
// never advertise a URL that wasn't generated. Format follows the llms.txt
// convention: a title, a summary, then one section per top-level doc area.
export default endpoint(async () => {
  const navTree = await getNavTree();

  const blocks: string[] = [`# Putnami`, ``, `> ${SUMMARY}`, ``, `## Start`, `- [Documentation hub](${BASE_URL}/docs)`];

  for (const section of navTree) {
    const lines: string[] = [];
    collectLinks([section], lines);
    if (lines.length === 0) continue;
    blocks.push(``, `## ${section.name}`, ...lines);
  }

  const body = `${blocks.join('\n')}\n`;

  return new HttpResponse(body, {
    headers: {
      'Content-Type': 'text/plain; charset=utf-8',
      'Cache-Control': 'public, max-age=3600',
    },
  });
});
