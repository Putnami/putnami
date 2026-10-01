import { island } from '@putnami/web';
import { ThemeProvider } from '@putnami/ui';
import { Navbar } from '../components/navbar';
import type { NavItem } from '../lib/docs/navigation';
import { putnamiTheme } from '../theme';

/**
 * Island wrapper for the shared navigation bar (mobile drawer + theme toggle).
 *
 * Islands are isolated React roots that do NOT inherit the layout's
 * ThemeProvider, so this wrapper supplies its own. The theme toggle reads/writes
 * the color mode from this provider's context and applies it globally by
 * toggling `document.documentElement` + localStorage (CSS-variable theming), so
 * a static page's theme stays correct with zero base JS — only this island
 * hydrates to make the toggle interactive.
 *
 * `navItems` is serialized into the island marker as props so the mobile menu
 * can render the docs tree after hydration.
 */
function NavbarIsland({ navItems }: { navItems?: NavItem[] }) {
  return (
    <ThemeProvider theme={putnamiTheme}>
      <Navbar navItems={navItems} />
    </ThemeProvider>
  );
}

export default island().load('load').render(NavbarIsland);
