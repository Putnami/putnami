'use client';

import createCache, { type EmotionCache } from '@emotion/cache';
import { CacheProvider, ThemeProvider as EmotionThemeProvider } from '@emotion/react';
import { useDocumentMeta } from '@putnami/web';
import { createContext, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  buildColorModeScript,
  isColorModeScript,
  parseStoredColorMode,
  resolveColorMode,
  STORAGE_KEY,
} from './color-mode';
import { createTheme } from './default';
import { GlobalStyles } from './global-style';
import type { ColorMode, ResolvedColorMode, Theme } from './theme';
import { deepMerge } from './utils';

const isServer = typeof window === 'undefined';

/** Context value providing color mode state and setter. */
export interface ColorModeContextValue {
  colorMode: ColorMode;
  setColorMode: (mode: ColorMode) => void;
  resolvedColorMode: ResolvedColorMode;
}

export const ColorModeContext = createContext<ColorModeContextValue | null>(null);

function getStoredColorMode(): ColorMode | null {
  if (typeof window === 'undefined') return null;
  return parseStoredColorMode(localStorage.getItem(STORAGE_KEY));
}

/**
 * Create an Emotion cache for SSR with compat mode enabled.
 * `compat: true` prevents Emotion from rendering inline `<style>` tags
 * alongside components during SSR, which would cause hydration mismatches.
 * Instead, styles are collected in `cache.inserted` and extracted into `<head>`.
 */
function createSsrEmotionCache(): { cache: EmotionCache; getStyleTags: () => Array<{ css: string; names: string }> } {
  const cache = createCache({ key: 'css' });
  cache.compat = true;

  const insertedNames: string[] = [];
  const originalInsert = cache.insert.bind(cache);
  cache.insert = (...args: Parameters<EmotionCache['insert']>) => {
    const serialized = args[1];
    if (cache.inserted[serialized.name] === undefined) {
      insertedNames.push(serialized.name);
    }
    return originalInsert(...args);
  };

  const getStyleTags = () => {
    const tags: Array<{ css: string; names: string }> = [];
    let css = '';
    const names: string[] = [];
    for (const name of insertedNames) {
      const inserted = cache.inserted[name];
      if (typeof inserted === 'string') {
        css += inserted;
        names.push(name);
      }
    }
    if (css) {
      tags.push({ css, names: names.join(' ') });
    }
    return tags;
  };

  return { cache, getStyleTags };
}

/** Props for the ThemeProvider component. */
export interface ThemeProviderProps {
  theme?: Partial<Theme> | ((baseTheme: Theme) => Theme);
  colorMode?: ColorMode;
  children: React.ReactNode;
}

/** Resolves a user theme override against a base theme for one color mode. */
export function resolveTheme(themeProp: ThemeProviderProps['theme'], colorMode: ResolvedColorMode): Theme {
  const baseTheme = createTheme(colorMode);

  if (!themeProp) {
    return baseTheme;
  }

  if (typeof themeProp === 'function') {
    return themeProp(baseTheme);
  }

  return deepMerge(baseTheme, themeProp);
}

/** Root provider that supplies the Emotion theme and color mode context. Handles SSR with per-request Emotion cache and color-mode flash prevention. */
export function ThemeProvider({ theme: themeProp, colorMode: colorModeProp, children }: ThemeProviderProps) {
  // Initialize color mode from prop or default to 'system'
  // We avoid reading from storage during initialization to prevent hydration mismatch
  const [colorMode, setColorModeState] = useState<ColorMode>(() => {
    if (colorModeProp) return colorModeProp;
    return 'system';
  });

  // Sync with storage on mount
  useEffect(() => {
    if (!colorModeProp) {
      const stored = getStoredColorMode();
      if (stored) {
        setColorModeState(stored);
      }
    }
  }, [colorModeProp]);

  // Initialize to 'light' to match the server render and avoid hydration mismatch.
  // The actual system preference is read in the useEffect below, after hydration.
  const [systemMode, setSystemMode] = useState<ResolvedColorMode>('light');

  // Read actual system color mode after hydration + listen for changes
  useEffect(() => {
    const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
    setSystemMode(mediaQuery.matches ? 'dark' : 'light');
    const handler = (e: MediaQueryListEvent) => {
      setSystemMode(e.matches ? 'dark' : 'light');
    };
    mediaQuery.addEventListener('change', handler);
    return () => mediaQuery.removeEventListener('change', handler);
  }, []);

  // Sync with controlled colorMode prop
  useEffect(() => {
    if (colorModeProp) {
      setColorModeState(colorModeProp);
    }
  }, [colorModeProp]);

  const resolvedColorMode: ResolvedColorMode = resolveColorMode(colorMode, systemMode);

  const setColorMode = useCallback((mode: ColorMode) => {
    setColorModeState(mode);
    if (typeof window !== 'undefined') {
      localStorage.setItem(STORAGE_KEY, mode);
    }
  }, []);

  // Resolve both palettes so GlobalStyles can switch custom CSS variables without
  // re-rendering components. A theme factory receives the matching base theme for
  // each mode, preserving mode-specific overrides.
  const { lightTheme, darkTheme, finalTheme } = useMemo(() => {
    const lightTheme = resolveTheme(themeProp, 'light');
    const darkTheme = resolveTheme(themeProp, 'dark');

    return {
      lightTheme,
      darkTheme,
      finalTheme: resolvedColorMode === 'dark' ? darkTheme : lightTheme,
    };
  }, [themeProp, resolvedColorMode]);

  const colorModeValue = useMemo(
    () => ({
      colorMode,
      setColorMode,
      resolvedColorMode,
    }),
    [colorMode, setColorMode, resolvedColorMode],
  );

  // --- Emotion SSR: create a per-request cache and register a style extractor ---
  const documentMeta = useDocumentMeta();
  const ssrCacheRef = useRef<ReturnType<typeof createSsrEmotionCache> | null>(null);

  if (isServer && !ssrCacheRef.current) {
    const ssrCache = createSsrEmotionCache();
    ssrCacheRef.current = ssrCache;

    if (documentMeta) {
      // Inject a blocking script that detects color mode before first paint.
      // This sets data-color-mode on <html> so the static CSS variables in
      // GlobalStyles apply the correct palette immediately, avoiding a flash
      // and ensuring Firefox/Safari render the right background.
      documentMeta.scripts ||= [];
      const hasColorModeScript = documentMeta.scripts.some(isColorModeScript);
      if (!hasColorModeScript) {
        documentMeta.scripts.unshift({ children: buildColorModeScript() });
      }

      // Register a lazy style extractor on DocumentMeta.
      // Called by SsrDocumentHelper.headHtml after allReady (all components rendered).
      documentMeta.styleExtractors ||= [];
      documentMeta.styleExtractors.push(() =>
        ssrCache.getStyleTags().map((tag) => ({
          dangerouslySetInnerHTML: { __html: tag.css },
          'data-emotion': `${ssrCache.cache.key} ${tag.names}`,
        })),
      );
    }
  }

  const emotionContent = (
    <EmotionThemeProvider theme={finalTheme}>
      <GlobalStyles lightTheme={lightTheme} darkTheme={darkTheme} />
      {children}
    </EmotionThemeProvider>
  );

  return (
    <ColorModeContext.Provider value={colorModeValue}>
      {ssrCacheRef.current ? (
        <CacheProvider value={ssrCacheRef.current.cache}>{emotionContent}</CacheProvider>
      ) : (
        emotionContent
      )}
    </ColorModeContext.Provider>
  );
}
