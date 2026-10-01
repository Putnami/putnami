import { describe, expect, it } from 'bun:test';
import type { ReactElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { specTest } from '@putnami/spectest';
import { ThemeProvider } from '@putnami/ui';
import DocEnhancerIsland from '../src/app/docs/[...page]/doc-enhancer.island';
import DocTocIsland from '../src/app/docs/[...page]/doc-toc.island';
import { HeroTerminal } from '../src/components/hero-terminal';
import { Navbar } from '../src/components/navbar';
import { Sidebar } from '../src/components/sidebar';
import { StatusDot } from '../src/components/tool-glyph';
import type { NavItem } from '../src/lib/docs/navigation';
import { getDocsPaths, getNavTree } from '../src/lib/docs/navigation.server';

// Static pages are pre-rendered at build, so the integration suite (which serves
// the prebuilt HTML) never exercises these shell render paths. The shell is
// router-free (it hydrates as isolated island roots), so a plain ThemeProvider
// is all that's needed to render it here — mirroring the build-time SSG pass.
function ssr(ui: ReactElement): string {
  return renderToStaticMarkup(<ThemeProvider colorMode='light'>{ui}</ThemeProvider>);
}

const navItems: NavItem[] = [
  {
    name: 'Getting Started',
    order: 1,
    contentPath: '01-getting-started',
    children: [
      {
        name: 'Introduction',
        order: 1,
        contentPath: '01-getting-started/01-introduction.md',
        children: [{ name: 'Details', order: 1, contentPath: '01-getting-started/01-introduction/01-details.md' }],
      },
    ],
  },
];

const productNavItems: NavItem[] = [
  {
    name: 'Getting Started',
    order: 1,
    contentPath: '01-getting-started',
  },
  {
    name: 'How To',
    order: 3,
    contentPath: '03-how-to',
    children: [
      { name: 'Build A Web App', order: 1, contentPath: '03-how-to/01-build-a-web-app.md' },
      { name: 'Build An Api Service', order: 2, contentPath: '03-how-to/02-build-an-api-service.md' },
      { name: 'Share Code Between Projects', order: 3, contentPath: '03-how-to/03-share-code-between-projects.md' },
    ],
  },
  {
    name: 'Tooling & Workspace',
    order: 5,
    contentPath: '05-tooling-&-workspace',
    children: [{ name: 'CLI', order: 2, contentPath: '05-tooling-&-workspace/02-cli.md' }],
  },
  {
    name: 'Frameworks',
    order: 6,
    contentPath: '06-frameworks',
    children: [
      {
        name: 'Typescript',
        order: 1,
        contentPath: '06-frameworks/01-typescript',
        children: [
          { name: 'Getting Started', order: 0, contentPath: '06-frameworks/01-typescript/00-getting-started.md' },
          {
            name: 'Extension',
            order: 0,
            contentPath: '06-frameworks/01-typescript/00-extension.md',
            children: [
              {
                name: 'Project Detection',
                order: 1,
                contentPath: '06-frameworks/01-typescript/00-extension/01-project-detection.md',
              },
            ],
          },
          { name: 'Overview', order: 1, contentPath: '06-frameworks/01-typescript/01-overview.md' },
          { name: 'Web', order: 2, contentPath: '06-frameworks/01-typescript/02-web.md' },
        ],
      },
      {
        name: 'Go',
        order: 2,
        contentPath: '06-frameworks/02-go',
        children: [{ name: 'Overview', order: 1, contentPath: '06-frameworks/02-go/01-overview.md' }],
      },
      {
        name: 'Python',
        order: 3,
        contentPath: '06-frameworks/03-python',
      },
    ],
  },
  {
    name: 'Platform',
    order: 7,
    children: [{ name: 'CI', order: 1, contentPath: '07-platform/ci' }],
  },
];

describe('router-free shell components', () => {
  it('renders the hero terminal chrome', () => {
    const html = ssr(<HeroTerminal />);
    expect(html).toContain('putnami — zsh');
  });

  it('renders the navbar with plain anchors (no router Link)', () => {
    const html = ssr(<Navbar navItems={navItems} />);
    expect(html).toContain('href="/docs"');
    expect(html).toContain('href="https://github.com/putnami/putnami"');
    expect(html).toContain('Search');
  });

  it('renders the navbar even without nav items', () => {
    const html = ssr(<Navbar />);
    expect(html).toContain('href="/"');
  });

  specTest(
    'lists Python inside navigation groups without creating a first-level Python anchor',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-browseable-not-anchored',
      check: 'python-is-listed-without-a-first-level-anchor',
    },
    async () => {
      const publishedPaths = new Set(await getDocsPaths());
      expect(publishedPaths.has('frameworks/python')).toBe(true);

      const html = ssr(<Navbar navItems={await getNavTree()} />);
      const pythonAnchors = html.match(/<a[^>]*href="\/docs\/frameworks\/python"[^>]*>/g) ?? [];
      expect(pythonAnchors.length).toBeGreaterThan(0);
      expect(pythonAnchors.every((anchor) => anchor.includes('menu-link'))).toBe(true);
      expect(html).toContain('Python');
    },
  );

  it('renders no status badge when a documented surface is unclassified', () => {
    expect(ssr(<StatusDot status={undefined} />)).toBe('');
  });

  it('renders the sidebar with the active link highlighted via currentPath', () => {
    const html = ssr(<Sidebar navItems={navItems} currentPath='/docs/getting-started/introduction' />);
    expect(html).toContain('href="/docs/getting-started/introduction"');
    expect(html).toContain('data-active="true"');
  });

  it('renders the sidebar with no active link when currentPath does not match', () => {
    const html = ssr(<Sidebar navItems={navItems} currentPath='/docs/elsewhere' />);
    expect(html).toContain('Start &amp; guides');
    expect(html).not.toContain('data-active="true"');
  });

  it('scopes framework language pages as first-level product surfaces', () => {
    const html = ssr(<Sidebar navItems={productNavItems} currentPath='/docs/frameworks/typescript/overview' />);
    expect(html).toContain('TypeScript');
    expect(html).toContain('language docs');
    expect(html).toContain('href="/docs/frameworks/typescript/overview"');
    expect(html).toContain('data-active="true"');
  });

  it('groups framework sidebars around start, guides, extension, and capabilities', () => {
    const html = ssr(<Sidebar navItems={productNavItems} currentPath='/docs/frameworks/typescript/overview' />);

    const start = html.indexOf('Getting Started');
    const guides = html.indexOf('How To / Guides');
    const extension = html.indexOf('Extension');
    const capabilities = html.indexOf('Framework / Capabilities');

    expect(start).toBeGreaterThan(-1);
    expect(guides).toBeGreaterThan(start);
    expect(extension).toBeGreaterThan(guides);
    expect(capabilities).toBeGreaterThan(extension);
    expect(html).toContain('href="/docs/frameworks/typescript/extension/project-detection"');
  });

  it('renders the doc enhancer island to nothing (behaviour attaches on hydration)', () => {
    // The doc body is emitted once as static HTML; this island ships no markup,
    // only the enhancement behaviour.
    expect(ssr(<DocEnhancerIsland />)).toBe('');
  });

  it('renders the doc toc island without inheriting the layout theme provider', () => {
    const html = renderToStaticMarkup(<DocTocIsland items={[{ level: 2, id: 'install', text: 'Install' }]} />);
    expect(html).toContain('On this page');
    expect(html).toContain('href="#install"');
  });
});
