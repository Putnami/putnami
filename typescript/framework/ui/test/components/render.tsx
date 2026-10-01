import type React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ThemeProvider } from '../../src/theme/provider';
import type { ColorMode } from '../../src/theme/theme';

export const render = (ui: React.ReactElement, colorMode: ColorMode = 'light'): string =>
  renderToStaticMarkup(<ThemeProvider colorMode={colorMode}>{ui}</ThemeProvider>);

/**
 * Renders `ui` in both color modes.
 *
 * Emotion hashes the serialized declarations into the class name, so a component
 * that bakes `theme.colors.*` emits a different class per mode. That class is
 * generated once during SSR — where the palette is always light — so the baked
 * literal survives every later mode flip. Styling from the CSS custom properties
 * makes both renders identical and leaves the color for the browser to resolve.
 */
export const renderBothModes = (ui: React.ReactElement): { light: string; dark: string } => ({
  light: render(ui, 'light'),
  dark: render(ui, 'dark'),
});
