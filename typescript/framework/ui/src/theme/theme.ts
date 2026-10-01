import type { Theme as EmotionTheme } from '@emotion/react';

/** User preference for light, dark, or system color mode. */
export type ColorMode = 'light' | 'dark' | 'system';
/** Actual resolved color mode after evaluating system preference. */
export type ResolvedColorMode = 'light' | 'dark';

/** Numeric scale (50-900) for a single color. */
export interface ColorPalette {
  50: string;
  100: string;
  200: string;
  300: string;
  400: string;
  500: string;
  600: string;
  700: string;
  800: string;
  900: string;
}

/** Semantic color with main, light, dark, and contrastText variants. */
export interface PaletteColor {
  main: string;
  light: string;
  dark: string;
  contrastText: string;
}

/** Full semantic color combining numeric scale and palette variants. */
export type SemanticColor = ColorPalette & PaletteColor;

/** Complete color system for the theme. */
export interface ThemeColors {
  white: string;
  black: string;
  gray: ColorPalette;
  primary: SemanticColor;
  secondary: SemanticColor;
  error: SemanticColor;
  success: SemanticColor;
  warning: SemanticColor;
  info: SemanticColor;
  text: {
    primary: string;
    secondary: string;
    disabled: string;
  };
  background: string;
  surface: string;
  surfaceHover: string;
  border: string;
}

/**
 * GitHub-dark inspired palette for the terminal surface.
 *
 * Mode-independent: the terminal is always dark, in both light and dark color
 * modes. Applied via the `.surface-terminal` class, which re-binds the semantic
 * color tokens to these values. Sourced from the hero terminal motif.
 */
export interface TerminalColors {
  /** Deepest background. */
  bg: string;
  /** Gradient terminus (lighter background). */
  bgAlt: string;
  /** Chrome bar / borders. */
  chrome: string;
  /** Default foreground text. */
  fg: string;
  /** Muted text, comments, dim output. */
  fgMuted: string;
  /** Prompt arrow and success lines (green). */
  prompt: string;
  /** Current working directory (cyan). */
  dir: string;
  /** `git:( )` annotator (blue). */
  git: string;
  /** Branch name (red — signals dirty/attention). */
  branch: string;
  /** Dirty marker `✗` (yellow). */
  dirty: string;
  /** Info `→` action (blue). */
  info: string;
  /** Warning traffic-light dot (amber). */
  warn: string;
}

/** Typography settings for a single heading level. */
export interface ThemeHeadingStyle {
  fontSize: string;
  marginTop: string;
  marginBottom: string;
}

/** Typography settings for all heading levels (h1-h6). */
export interface ThemeHeadings {
  fontWeight: number;
  lineHeight: number;
  h1: ThemeHeadingStyle;
  h2: ThemeHeadingStyle;
  h3: ThemeHeadingStyle;
  h4: ThemeHeadingStyle;
  h5: ThemeHeadingStyle;
  h6: ThemeHeadingStyle;
}

/** Complete typography system including fonts, sizes, weights, line heights, and headings. */
export interface ThemeTypography {
  fontFamily: {
    sans: string;
    mono: string;
  };
  fontSizes: {
    xs: string;
    sm: string;
    md: string;
    lg: string;
    xl: string;
    '2xl': string;
    '3xl': string;
    '4xl': string;
  };
  fontWeights: {
    regular: number;
    medium: number;
    semibold: number;
    bold: number;
  };
  lineHeights: {
    tight: number;
    normal: number;
    relaxed: number;
  };
  headings: ThemeHeadings;
}

/** Complete resolved theme object provided via Emotion's ThemeProvider. */
export interface Theme extends EmotionTheme {
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
  /** GitHub-dark palette for the `.surface-terminal` surface (mode-independent). */
  terminal: TerminalColors;
}
