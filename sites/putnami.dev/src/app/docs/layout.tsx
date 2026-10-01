import { layout, Outlet, useLoaderData, useLocation } from '@putnami/web';
import { Box, Flex } from '@putnami/ui';
import { Sidebar } from '../../components/sidebar';
import type { NavItem } from '../../lib/docs/navigation';
import { DOCS_STICKY_TOP } from '../../theme';

interface LoaderData {
  navItems: NavItem[];
}

export default layout().render(() => {
  const { navItems } = useLoaderData<LoaderData>();
  // The docs layout is part of the static prerender (it renders inside the
  // build-time StaticRouterProvider), so useLocation resolves to the page's own
  // URL and the active link is baked per static page. The desktop sidebar ships
  // zero JS — no island needed.
  const { pathname } = useLocation();

  // The /docs hub is the un-blobbed front door: tool cards, not a global tree.
  // It renders full-width; the scoped sidebar only appears once you're inside a
  // tool/section, where local wayfinding is what you actually need.
  const isHub = pathname === '/docs' || pathname === '/docs/';

  if (isHub) {
    return (
      <Box flex={1} style={{ marginTop: 12 }}>
        <Outlet />
      </Box>
    );
  }

  return (
    <Flex flex={1}>
      {/* Sticky rail: pinned below the fixed navbar, scrolling on its own when
          the tree is taller than the viewport instead of scrolling away with
          the page. The 260px rail leaves too little content room below the md
          breakpoint; the navbar drawer carries the same tree there. */}
      <Box
        display={['none', 'none', 'block']}
        height='fit-content'
        position='sticky'
        style={{
          top: DOCS_STICKY_TOP,
          maxHeight: `calc(100vh - ${DOCS_STICKY_TOP}px)`,
          overflowY: 'auto',
          scrollbarWidth: 'thin',
          paddingBottom: 'var(--space-lg)',
          marginRight: 'var(--space-xl)',
        }}
      >
        <Sidebar navItems={navItems} currentPath={pathname} />
      </Box>
      <Box flex={1} style={{ marginTop: 12 }}>
        <Outlet />
      </Box>
    </Flex>
  );
});
