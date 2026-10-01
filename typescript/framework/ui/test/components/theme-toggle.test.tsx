import { describe, expect, test } from 'bun:test';
import type React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ThemeToggle } from '../../src/components/theme-toggle';
import { ThemeProvider } from '../../src/theme/provider';

const renderWithTheme = (ui: React.ReactElement) =>
  renderToStaticMarkup(<ThemeProvider colorMode='light'>{ui}</ThemeProvider>);

describe('ThemeToggle', () => {
  test('renders toggle button with accessible label', () => {
    const html = renderWithTheme(<ThemeToggle />);
    expect(html).toContain('aria-label="Toggle theme');
    expect(html).toContain('type="button"');
  });
});
