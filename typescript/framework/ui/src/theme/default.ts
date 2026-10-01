import type { ResolvedColorMode, SemanticColor, TerminalColors, Theme, ThemeColors } from './theme';

// Fresh teal accent for primary interactions
// main uses 700 shade for WCAG AA contrast (≥ 4.5:1 on white)
const teal: SemanticColor = {
  50: '#f0fdfa',
  100: '#ccfbf1',
  200: '#99f6e4',
  300: '#5eead4',
  400: '#2dd4bf',
  500: '#14b8a6',
  600: '#0d9488',
  700: '#0f766e',
  800: '#115e59',
  900: '#134e4a',
  main: '#14b8a6',
  light: '#0d9488',
  dark: '#115e59',
  contrastText: '#ffffff',
};

const neutral: SemanticColor = {
  50: '#fafafa',
  100: '#f5f5f5',
  200: '#e5e5e5',
  300: '#d4d4d4',
  400: '#a3a3a3',
  500: '#737373',
  600: '#525252',
  700: '#404040',
  800: '#262626',
  900: '#171717',
  main: '#e5e5e5',
  light: '#f5f5f5',
  dark: '#d4d4d4',
  contrastText: '#171717',
};

const red: SemanticColor = {
  50: '#fef2f2',
  100: '#fee2e2',
  200: '#fecaca',
  300: '#fca5a5',
  400: '#f87171',
  500: '#ef4444',
  600: '#dc2626',
  700: '#b91c1c',
  800: '#991b1b',
  900: '#7f1d1d',
  main: '#ef4444',
  light: '#f87171',
  dark: '#dc2626',
  contrastText: '#ffffff',
};

const emerald: SemanticColor = {
  50: '#ecfdf5',
  100: '#d1fae5',
  200: '#a7f3d0',
  300: '#6ee7b7',
  400: '#34d399',
  500: '#10b981',
  600: '#059669',
  700: '#047857',
  800: '#065f46',
  900: '#064e3b',
  main: '#10b981',
  light: '#34d399',
  dark: '#059669',
  contrastText: '#ffffff',
};

const amber: SemanticColor = {
  50: '#fffbeb',
  100: '#fef3c7',
  200: '#fde68a',
  300: '#fcd34d',
  400: '#fbbf24',
  500: '#f59e0b',
  600: '#d97706',
  700: '#b45309',
  800: '#92400e',
  900: '#78350f',
  main: '#f59e0b',
  light: '#fbbf24',
  dark: '#d97706',
  contrastText: '#000000',
};

const sky: SemanticColor = {
  50: '#f0f9ff',
  100: '#e0f2fe',
  200: '#bae6fd',
  300: '#7dd3fc',
  400: '#38bdf8',
  500: '#0ea5e9',
  600: '#0284c7',
  700: '#0369a1',
  800: '#075985',
  900: '#0c4a6e',
  main: '#0ea5e9',
  light: '#38bdf8',
  dark: '#0284c7',
  contrastText: '#ffffff',
};

// GitHub-dark inspired palette for the terminal surface.
// Mode-independent — the terminal is always dark. The hero terminal motif is
// promoted to a first-class surface via the `.surface-terminal` class, which
// re-binds the semantic color tokens to these values.
const terminalColors: TerminalColors = {
  bg: '#0d1117',
  bgAlt: '#161b22',
  chrome: '#21262d',
  fg: '#e6edf3',
  fgMuted: '#8b949e',
  prompt: '#3fb950',
  dir: '#39c5cf',
  git: '#79c0ff',
  branch: '#f85149',
  dirty: '#e3b341',
  info: '#58a6ff',
  warn: '#f0b429',
};

/** Default light mode color palette. */
export const lightColors: ThemeColors = {
  white: '#ffffff',
  black: '#000000',
  gray: neutral,
  primary: teal,
  secondary: neutral,
  error: red,
  success: emerald,
  warning: amber,
  info: sky,
  text: {
    primary: '#18181b',
    secondary: '#52525b',
    disabled: '#a1a1aa',
  },
  background: '#ffffff',
  surface: '#fafafa',
  surfaceHover: '#f4f4f5',
  border: '#e4e4e7',
};

/** Default dark mode color palette. */
export const darkColors: ThemeColors = {
  white: '#ffffff',
  black: '#000000',
  gray: neutral,
  primary: {
    ...teal,
    main: '#2dd4bf',
    light: '#5eead4',
    dark: '#14b8a6',
    contrastText: '#042f2e',
  },
  secondary: {
    ...neutral,
    main: '#404040',
    light: '#525252',
    dark: '#262626',
    contrastText: '#fafafa',
  },
  error: {
    ...red,
    main: '#f87171',
    light: '#fca5a5',
    dark: '#ef4444',
  },
  success: {
    ...emerald,
    main: '#34d399',
    light: '#6ee7b7',
    dark: '#10b981',
  },
  warning: {
    ...amber,
    main: '#fbbf24',
    light: '#fcd34d',
    dark: '#f59e0b',
  },
  info: {
    ...sky,
    main: '#38bdf8',
    light: '#7dd3fc',
    dark: '#0ea5e9',
  },
  text: {
    primary: '#fafafa',
    secondary: '#a1a1aa',
    disabled: '#52525b',
  },
  background: '#0a0a0b',
  surface: '#18181b',
  surfaceHover: '#27272a',
  border: '#27272a',
};

// Base theme without colors (shared between light and dark)
const baseTheme = {
  spacing: (factor: number) => `${factor * 0.25}rem`, // 4px base
  space: {
    xs: '0.25rem', // 4px
    sm: '0.5rem', // 8px
    md: '1rem', // 16px
    lg: '1.5rem', // 24px
    xl: '2rem', // 32px
    xxl: '3rem', // 48px
  },
  breakpoints: {
    sm: 640,
    md: 768,
    lg: 1024,
    xl: 1280,
    xxl: 1536,
  },
  typography: {
    fontFamily: {
      sans: '"Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
      mono: '"JetBrains Mono", monospace',
    },
    fontSizes: {
      xs: '0.75rem',
      sm: '0.875rem',
      md: '1rem',
      lg: '1.125rem',
      xl: '1.25rem',
      '2xl': '1.5rem',
      '3xl': '1.875rem',
      '4xl': '2.25rem',
    },
    fontWeights: {
      regular: 400,
      medium: 500,
      semibold: 600,
      bold: 700,
    },
    lineHeights: {
      tight: 1.25,
      normal: 1.5,
      relaxed: 1.75,
    },
    headings: {
      fontWeight: 700,
      lineHeight: 1.25,
      h1: { fontSize: '2.25rem', marginTop: '1.5em', marginBottom: '0.5em' },
      h2: { fontSize: '1.875rem', marginTop: '1.5em', marginBottom: '0.5em' },
      h3: { fontSize: '1.5rem', marginTop: '1.5em', marginBottom: '0.5em' },
      h4: { fontSize: '1.25rem', marginTop: '1.5em', marginBottom: '0.5em' },
      h5: { fontSize: '1.125rem', marginTop: '1.5em', marginBottom: '0.5em' },
      h6: { fontSize: '1rem', marginTop: '1.5em', marginBottom: '0.5em' },
    },
  },
  shadows: {
    sm: '0 1px 2px 0 rgba(0, 0, 0, 0.05)',
    md: '0 4px 6px -1px rgba(0, 0, 0, 0.1), 0 2px 4px -1px rgba(0, 0, 0, 0.06)',
    lg: '0 10px 15px -3px rgba(0, 0, 0, 0.1), 0 4px 6px -2px rgba(0, 0, 0, 0.05)',
    xl: '0 20px 25px -5px rgba(0, 0, 0, 0.1), 0 10px 10px -5px rgba(0, 0, 0, 0.04)',
  },
  radii: {
    sm: '0.125rem',
    md: '0.375rem',
    lg: '0.5rem',
    full: '9999px',
  },
  terminal: terminalColors,
};

/** Creates a complete theme for the given color mode. */
export function createTheme(colorMode: ResolvedColorMode): Theme {
  return {
    ...baseTheme,
    colorMode,
    colors: colorMode === 'dark' ? darkColors : lightColors,
  };
}

/** Pre-built light mode theme. */
export const defaultTheme: Theme = createTheme('light');

/** Pre-built dark mode theme. */
export const darkTheme: Theme = createTheme('dark');
