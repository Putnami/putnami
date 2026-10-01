/**
 * Re-exports Emotion primitives (`styled`, `Global`, `css`, `keyframes`) and augments the
 * Emotion `Theme` interface with Putnami's theme shape.
 */
import { css, Global, keyframes } from '@emotion/react';
import styled from '@emotion/styled';
import type { ResolvedColorMode, TerminalColors, ThemeColors, ThemeTypography } from '../theme/theme';

declare module '@emotion/react' {
  export interface Theme {
    colorMode: ResolvedColorMode;
    colors: ThemeColors;
    spacing: (factor: number) => string;
    space: {
      xs: string;
      sm: string;
      md: string;
      lg: string;
      xl: string;
      xxl: string;
    };
    breakpoints: {
      sm: number;
      md: number;
      lg: number;
      xl: number;
      xxl: number;
    };
    typography: ThemeTypography;
    shadows: {
      sm: string;
      md: string;
      lg: string;
      xl: string;
    };
    radii: {
      sm: string;
      md: string;
      lg: string;
      full: string;
    };
    terminal: TerminalColors;
  }
}

export { styled, Global, css, keyframes };
