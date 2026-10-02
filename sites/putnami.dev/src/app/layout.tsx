import { Favicon, HeaderLink, layout, Meta, Outlet, Style, useLoaderData } from '@putnami/web';
import { Box, Container, ThemeProvider } from '@putnami/ui';
import { PageMeta } from '../components/page-meta';
import type { NavItem } from '../lib/docs/navigation';
import { putnamiTheme, SITE_MAX_WIDTH } from '../theme';
import { Footer } from '../components/footer';
import NavbarIsland from './navbar.island';
import ReleaseVersionIsland from './release-version.island';
import SearchIsland from './search.island';

export default layout().render(() => {
  const { navItems } = useLoaderData<{ navItems: NavItem[] }>() || {};

  return (
    <ThemeProvider theme={putnamiTheme}>
      {/* Default meta tags - pages override these with their own PageMeta */}
      <PageMeta
        title='Putnami — Branch-native workspace tooling for TypeScript'
        description='Local-first monorepo tooling where every branch is a runnable environment. Workspace orchestration, polyglot framework support, and preview deployments — designed for humans and AI agents.'
      />
      <Meta name='viewport' content='width=device-width, initial-scale=1' />
      <Favicon href='/assets/favico.svg' />
      <HeaderLink rel='icon' type='image/x-icon' href='/favicon.ico' />
      {HeaderLink({
        rel: 'preload',
        href: '/fonts/inter-latin.woff2',
        as: 'font',
        type: 'font/woff2',
        crossOrigin: 'anonymous',
      })}
      {Style({
        children: `@font-face {
  font-family: 'Inter';
  font-style: normal;
  font-weight: 100 900;
  font-display: swap;
  src: url(/fonts/inter-latin-ext.woff2) format('woff2');
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: 'Inter';
  font-style: normal;
  font-weight: 100 900;
  font-display: swap;
  src: url(/fonts/inter-latin.woff2) format('woff2');
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
/* Island host markers carry no default box; make them layout-neutral block
   elements so wrapping a component in an island never collapses a Grid/flex
   item. Block (not display:contents) keeps an observable box for the
   'visible' hydration strategy's IntersectionObserver. */
putnami-island,
putnami-island-root {
  display: block;
}
/* The release version is a run of text inside the footer line. */
putnami-island[data-island='release-version'],
putnami-island[data-island='release-version'] > putnami-island-root {
  display: inline;
}`,
      })}

      <NavbarIsland navItems={navItems || []} />
      <SearchIsland />
      <Box pt={['80px', '96px']}>
        <Container maxWidth={SITE_MAX_WIDTH}>
          <Outlet />
        </Container>
        <Footer release={<ReleaseVersionIsland />} />
      </Box>
    </ThemeProvider>
  );
});
