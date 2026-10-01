import { Link, page, useLoaderData } from '@putnami/web';
import { Box, Breadcrumb, BreadcrumbItem, Flex, Grid, MarkdownRenderer, styled } from '@putnami/ui';
import { BASE_URL, PageMeta } from '../../../components/page-meta';
import { DOCS_STICKY_TOP } from '../../../theme';
import CopyMarkdownIsland from './copy-markdown.island';
import DocEnhancerIsland from './doc-enhancer.island';
import DocTocIsland from './doc-toc.island';
import type { DocLoaderData } from './loader';

// Materialize every lock-pinned content bundle before enumerating doc paths.
// The web generate hook runs before the application generate hook, so doing
// this inside the static-path callback makes a cold build complete in one pass.
// Imported lazily via a variable specifier so the server-only filesystem code
// is never pulled into the client page bundle.
const STATIC_PATHS_SERVER_MODULE = '../../../lib/docs/static-paths.server';
async function docsStaticPaths(): Promise<Array<{ '*': string }>> {
  const { getMaterializedDocsPaths } = (await import(
    STATIC_PATHS_SERVER_MODULE
  )) as typeof import('../../../lib/docs/static-paths.server');
  return (await getMaterializedDocsPaths()).map((splat) => ({ '*': splat }));
}

function buildJsonLd(title: string, description: string, path: string[]) {
  const pageUrl = `${BASE_URL}/docs/${path.join('/')}`;

  const breadcrumbItems = [
    { '@type': 'ListItem', position: 1, name: 'Docs', item: `${BASE_URL}/docs` },
    ...path.map((segment, index) => ({
      '@type': 'ListItem',
      position: index + 2,
      name: formatSegment(segment),
      item: `${BASE_URL}/docs/${path.slice(0, index + 1).join('/')}`,
    })),
  ];

  return {
    '@context': 'https://schema.org',
    '@graph': [
      {
        '@type': 'Article',
        headline: title,
        description,
        url: pageUrl,
        publisher: { '@type': 'Organization', name: 'Putnami', url: BASE_URL },
      },
      {
        '@type': 'BreadcrumbList',
        itemListElement: breadcrumbItems,
      },
    ],
  };
}

// Pre-rendered at build (SSG) and revalidated on the cloud runtime: by TTL and
// when the `docs` tag is revalidated after a docs rebuild. Zero base JS — only
// the doc body (copy buttons, mermaid, code-group tabs) and the TOC scroll-spy
// hydrate as islands.
export default page()
  .static({ paths: docsStaticPaths, revalidate: { seconds: 3600 * 24, tags: ['docs'] } })
  .render(() => {
    const { html, toc, path, title, description } = useLoaderData<DocLoaderData>();

    return (
      <Box as='main'>
        <PageMeta
          title={`${title} — Putnami Docs`}
          description={description}
          url={`/docs/${path.join('/')}`}
          type='article'
          jsonLd={buildJsonLd(title, description, path)}
        />
        <Flex mb='lg' alignItems='center' justifyContent='space-between' gap='md' wrap='wrap'>
          <Breadcrumb>
            <BreadcrumbItem>
              <BreadcrumbLink to='/docs'>Docs</BreadcrumbLink>
            </BreadcrumbItem>
            {path.map((segment, index) => (
              <BreadcrumbItem key={segment}>
                {index === path.length - 1 ? (
                  <BreadcrumbCurrent>{formatSegment(segment)}</BreadcrumbCurrent>
                ) : (
                  <BreadcrumbLink to={`/docs/${path.slice(0, index + 1).join('/')}`}>
                    {formatSegment(segment)}
                  </BreadcrumbLink>
                )}
              </BreadcrumbItem>
            ))}
          </Breadcrumb>
          <CopyMarkdownIsland />
        </Flex>

        <Grid columns={['1fr', '1fr', '1fr', '1fr 260px']} gap='2xl' alignItems='start'>
          {/* Doc body emitted once as static HTML; the enhancer island below
              attaches copy buttons / mermaid / code-group tabs in place. */}
          <MarkdownRenderer html={html} />
          {toc.length > 0 && (
            <TocRail>
              <DocTocIsland items={toc} />
            </TocRail>
          )}
        </Grid>
        <DocEnhancerIsland />
      </Box>
    );
  });

function formatSegment(segment: string): string {
  return segment
    .split('-')
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

const BreadcrumbLink = styled(Link)`
  color: var(--color-text-muted);
  text-decoration: none;
  transition: color var(--transition-fast);

  &:hover {
    color: var(--color-text);
  }
`;

const BreadcrumbCurrent = styled.span`
  color: var(--color-text);
  font-weight: 500;
`;

/*
 * Sticky rail for the on-this-page TOC. The island host element is a plain
 * block only as tall as its content, so the TOC component's own sticky
 * positioning can never engage inside it — the rail owns stickiness (and
 * independent scrolling for long TOCs) and flattens the nav inside.
 * Hidden below the lg breakpoint, where the grid collapses to one column and
 * the TOC itself is display: none.
 */
const TocRail = styled.aside`
  display: none;

  @media (min-width: 1024px) {
    display: block;
    position: sticky;
    top: ${DOCS_STICKY_TOP}px;
    max-height: calc(100vh - ${DOCS_STICKY_TOP}px);
    overflow-y: auto;
    scrollbar-width: thin;
    padding-bottom: var(--space-lg);
  }

  & nav {
    position: static;
    margin-top: 0;
  }
`;
