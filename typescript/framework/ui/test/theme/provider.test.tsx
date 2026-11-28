import { afterEach, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { renderToStaticMarkup } from 'react-dom/server';
import { GlobalStyles, semanticColorVars } from '../../src/theme/global-style';
import { buildColorModeScript } from '../../src/theme/color-mode';
import { resolveTheme, ThemeProvider } from '../../src/theme/provider';
import type { Theme } from '../../src/theme/theme';
import { setMockDocumentMeta } from '../setup';

describe('ThemeProvider', () => {
  afterEach(() => {
    setMockDocumentMeta(null);
  });

  specTest(
    'emits custom color variables for both palettes from a theme factory',
    {
      feature: 'typescript/ui-system',
      requirement: 'mode-reactive-colors',
      check: 'both-palettes-emit-custom-color-variables',
    },
    () => {
      const theme = (baseTheme: Theme): Theme => ({
        ...baseTheme,
        colors: {
          ...baseTheme.colors,
          background: baseTheme.colorMode === 'light' ? '#fefce8' : '#172554',
          error: {
            ...baseTheme.colors.error,
            main: baseTheme.colorMode === 'light' ? '#b91c1c' : '#fca5a5',
          },
          text: {
            ...baseTheme.colors.text,
            primary: baseTheme.colorMode === 'light' ? '#422006' : '#fef3c7',
          },
        },
      });
      const lightCss = semanticColorVars(resolveTheme(theme, 'light').colors);
      const darkCss = semanticColorVars(resolveTheme(theme, 'dark').colors);

      expect(lightCss).toContain('--color-bg: #fefce8;');
      expect(lightCss).toContain('--color-text: #422006;');
      expect(lightCss).toContain('--color-error: #b91c1c;');
      expect(darkCss).toContain('--color-bg: #172554;');
      expect(darkCss).toContain('--color-text: #fef3c7;');
      expect(darkCss).toContain('--color-error: #fca5a5;');
    },
  );

  test('supports rendering GlobalStyles without palette props', () => {
    expect(() =>
      renderToStaticMarkup(
        <ThemeProvider colorMode='light'>
          <GlobalStyles />
        </ThemeProvider>,
      ),
    ).not.toThrow();
  });

  specTest(
    'registers exactly one blocking color-mode script during SSR',
    {
      feature: 'typescript/ui-system',
      requirement: 'ssr-color-mode',
      check: 'the-bootstrap-script-is-registered-exactly-once-during-ssr',
    },
    () => {
      const documentMeta: {
        scripts?: Array<{ children?: unknown }>;
        styleExtractors?: Array<() => unknown[]>;
      } = {};
      setMockDocumentMeta(documentMeta);

      renderToStaticMarkup(<ThemeProvider colorMode='system'>first</ThemeProvider>);
      renderToStaticMarkup(<ThemeProvider colorMode='system'>second</ThemeProvider>);

      expect(documentMeta.scripts).toHaveLength(1);
      expect(documentMeta.scripts?.[0]?.children).toBe(buildColorModeScript());
      expect(documentMeta.styleExtractors).toHaveLength(2);
    },
  );
  test('the SSR style extractor returns only what was inserted since its previous call', async () => {
    const run = Bun.spawn([process.execPath, `${import.meta.dir}/../fixtures/ssr-style-extractor.tsx`], {
      cwd: `${import.meta.dir}/../..`,
      stdout: 'pipe',
      stderr: 'pipe',
    });
    const [out, err, code] = await Promise.all([
      new Response(run.stdout).text(),
      new Response(run.stderr).text(),
      run.exited,
    ]);
    expect(code, err).toBe(0);
    const [atShell, again, late, last] = JSON.parse(out) as unknown[];

    expect(JSON.stringify(atShell)).toContain('rgb(1, 2, 3)');
    expect(JSON.stringify(atShell)).not.toContain('rgb(4, 5, 6)');
    expect(again).toEqual([]);
    expect(JSON.stringify(late)).toContain('rgb(4, 5, 6)');
    expect(JSON.stringify(late)).not.toContain('rgb(1, 2, 3)');
    expect(last).toEqual([]);
  });
});
