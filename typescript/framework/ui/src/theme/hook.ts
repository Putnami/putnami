'use client';

import { useTheme as useEmotionTheme, withTheme as withEmotionTheme } from '@emotion/react';
import { useContext } from 'react';
import { ColorModeContext, type ColorModeContextValue } from './provider';
import type { Theme } from './theme';

/** Hook to access the current theme. Returns the Emotion theme cast to the generic type. */
export const useTheme = <T extends Theme>() => useEmotionTheme() as T;

/** HOC that injects the current theme as a prop. */
export const withTheme = <T extends Theme>(Component: React.ComponentType<T>) => withEmotionTheme(Component);

/** Hook to read and set the current color mode. Must be used within ThemeProvider. */
export function useColorMode(): ColorModeContextValue {
  const context = useContext(ColorModeContext);
  if (!context) {
    throw new Error('useColorMode must be used within a ThemeProvider');
  }
  return context;
}
