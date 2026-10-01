# Theming

The theme system controls colors, spacing, typography, breakpoints, shadows, and border radii across all components. It supports light/dark mode with automatic persistence and SSR-safe rendering.

## Overview

- Centralized design tokens consumed by all components via Emotion's `ThemeProvider`
- Built-in light and dark color palettes with full 50-900 shade scales
- Color mode switching with `system` preference support
- SSR-compatible: blocking script prevents flash of wrong theme
- Customizable via partial overrides or a theme factory function

## Usage

### Basic Setup

```typescript
import { ThemeProvider } from '@putnami/ui';

function App() {
  return (
    <ThemeProvider>
      {/* All components have access to the theme */}
    </ThemeProvider>
  );
}
```

### Custom Theme

Override specific tokens with a partial theme object:

```typescript
<ThemeProvider theme={{
  colors: {
    primary: {
      ...defaultTheme.colors.primary,
      main: '#6366f1', // Use indigo instead of teal
    },
  },
  typography: {
    fontFamily: {
      sans: '"Poppins", sans-serif',
      mono: '"Fira Code", monospace',
    },
  },
}}>
```

Or use a function for full control:

```typescript
<ThemeProvider theme={(baseTheme) => ({
  ...baseTheme,
  space: { ...baseTheme.space, md: '1.25rem' },
})}>
```

### Controlling Color Mode

```typescript
<ThemeProvider colorMode="dark">
  {/* Forces dark mode */}
</ThemeProvider>
```

Without `colorMode`, the provider defaults to `system` and reads from `localStorage` after hydration.

## API Reference

### `ThemeProvider`

| Prop | Type | Default | Description |
|------|------|---------|-------------|
| `theme` | `Partial<Theme> \| (base: Theme) => Theme` | — | Custom theme overrides or factory |
| `colorMode` | `'light' \| 'dark' \| 'system'` | `'system'` | Controlled color mode |
| `children` | `ReactNode` | — | Application content |

### `useTheme<T>()`

Returns the current Emotion theme, typed as `T`. Use `useTheme<Theme>()` to get the full typed theme object.

```typescript
import { useTheme } from '@putnami/ui';
import type { Theme } from '@putnami/ui';

function MyComponent() {
  const theme = useTheme<Theme>();
  return <div style={{ color: theme.colors.primary.main }} />;
}
```

### `useColorMode()`

Returns the current color mode and a setter.

```typescript
import { useColorMode } from '@putnami/ui';

function ModeSwitch() {
  const { colorMode, setColorMode, resolvedColorMode } = useColorMode();
  // colorMode: 'light' | 'dark' | 'system'
  // resolvedColorMode: 'light' | 'dark' (actual applied mode)
  // setColorMode: persists to localStorage
}
```

### `createTheme(colorMode)`

Creates a full theme object for the given color mode.

```typescript
import { createTheme } from '@putnami/ui';

const darkTheme = createTheme('dark');
```

### `ThemeToggle`

A ready-made button that cycles through light, dark, and system modes.

```typescript
import { ThemeToggle } from '@putnami/ui';

<ThemeToggle />
```

## Theme Structure

### Colors

```typescript
interface ThemeColors {
  white: string;
  black: string;
  gray: ColorPalette;        // 50-900 scale
  primary: SemanticColor;     // 50-900 + main/light/dark/contrastText
  secondary: SemanticColor;
  error: SemanticColor;
  success: SemanticColor;
  warning: SemanticColor;
  info: SemanticColor;
  text: { primary, secondary, disabled };
  background: string;
  surface: string;
  surfaceHover: string;
  border: string;
}
```

Default primary color is **teal** (`#14b8a6` light, `#2dd4bf` dark).

### Spacing

| Token | Value |
|-------|-------|
| `xs` | `0.25rem` (4px) |
| `sm` | `0.5rem` (8px) |
| `md` | `1rem` (16px) |
| `lg` | `1.5rem` (24px) |
| `xl` | `2rem` (32px) |
| `xxl` | `3rem` (48px) |

Numeric spacing: `theme.spacing(n)` returns `n * 0.25rem`.

### Typography

| Size | Value |
|------|-------|
| `xs` | `0.75rem` |
| `sm` | `0.875rem` |
| `md` | `1rem` |
| `lg` | `1.125rem` |
| `xl` | `1.25rem` |
| `2xl` | `1.5rem` |
| `3xl` | `1.875rem` |
| `4xl` | `2.25rem` |

Font families: Inter (sans), JetBrains Mono (mono).

### Breakpoints

| Name | Width |
|------|-------|
| `sm` | 640px |
| `md` | 768px |
| `lg` | 1024px |
| `xl` | 1280px |
| `xxl` | 1536px |

## CSS Custom Properties

`GlobalStyles` injects CSS variables that components use for colors and spacing. This enables dark mode switching without re-renders:

- `--color-bg`, `--color-surface`, `--color-border`, `--color-text`, etc.
- Raw color ramps `--teal-50` … `--teal-900` (and `--neutral-*`, `--red-*`, `--emerald-*`, `--amber-*`, `--sky-*`) — for charts and data viz; prefer the semantic `--color-*` tokens in components
- `--space-xs` through `--space-3xl`
- `--radius-sm` through `--radius-xl`
- `--shadow-sm` through `--shadow-xl`
- `--font-sans`, `--font-mono`
- Type ramp `--fs-xs` … `--fs-4xl`, weights `--fw-regular|medium|semibold|bold`, line heights `--lh-tight|normal|relaxed`
- `--bp-sm` … `--bp-xxl` (breakpoints) and `--container-prose|hero|grid|pricing|app|wide` (max-widths)
- `--icon-xs` … `--icon-xl` (icon sizes) and `--control-h|px|fs-{sm,md,lg}` (control density)
- `--transition-fast` (150ms), `--transition-base` (250ms), `--transition-slow` (350ms)
- Easings `--ease-out|in|in-out|linear` and overlay choreography `--motion-enter` (200ms), `--motion-exit` (150ms)
- Chart palette `--chart-1` … `--chart-8`, `--chart-pos|neg|grid|axis`
- Syntax tokens `--tok-keyword|string|number|comment|function|…` for `<pre class="code-block">`

Dark mode overrides (semantic colors and the chart palette) are applied via `html[data-color-mode='dark']`. The raw ramps, terminal palette, type ramp, spacing, motion, and layout tokens are mode-independent.

**Style mode-dependent colors from these properties, never from `theme.colors.*`.** Emotion serialises a styled component's declarations into its class name once, during SSR, where the palette always resolves to light — a baked literal stays light through hydration and every later toggle. A `var(--color-*)` reference is re-resolved by the browser when `data-color-mode` flips. Reading mode-independent values (`theme.space`, `theme.radii`, `theme.typography`, `theme.breakpoints`, and the raw ramps) from the theme is safe.

### Utility classes

`GlobalStyles` also ships a few utility classes:

- `.surface-terminal` — the terminal surface (see below)
- `.section-label` — the uppercase teal section kicker used on marketing pages
- `.display` — responsive hero heading (`clamp` across the `2xl → 4xl` ramp)
- `.skip-link` — a visually-hidden-until-focused accessibility skip link; place once at the top of `<body>`
- `.skeleton` — a calm loading-placeholder pulse (honors `prefers-reduced-motion`)
- `pre.code-block` + `.tok-*` — opt-in syntax highlighting under the GitHub-dark palette

### Accessibility

- **Focus-visible:** `:focus` paints nothing; `:focus-visible` paints a 2px primary outline (offset 2px). Inputs opt out and supply their own 1px inner ring. Mouse clicks never paint a ring; keyboard navigation always does.
- **Reduced motion:** a `@media (prefers-reduced-motion: reduce)` block collapses every transition and animation to near-zero.

## Terminal surface

The hero terminal motif is promoted to a first-class, reusable surface. Apply `class="surface-terminal"` (or `className="surface-terminal"`) to any container and it adopts the GitHub-dark CLI aesthetic — it is mode-independent (always dark) and re-binds the semantic color tokens (`--color-bg`, `--color-surface`, `--color-text`, …) to the terminal palette, so nested components inherit the dark surface automatically.

```tsx
<div className="surface-terminal" style={{ padding: 'var(--space-md)' }}>
  <span style={{ color: 'var(--term-prompt)' }}>➜</span>{' '}
  <span style={{ color: 'var(--term-dir)' }}>~/app</span>{' '}
  putnami serve
</div>
```

The palette is exposed both as CSS variables (`--term-bg`, `--term-bg-2`, `--term-chrome`, `--term-fg`, `--term-fg-muted`, `--term-prompt`, `--term-dir`, `--term-git`, `--term-branch`, `--term-dirty`, `--term-info`, `--term-warn`) and on the theme object at `theme.terminal`.

```typescript
const theme = useTheme<Theme>();
theme.terminal.prompt; // '#3fb950'
```

## Utilities

### `deepMerge(target, source)`

Deep-merges two objects. Used internally to merge partial themes.

### `getColor(theme, value)`

Resolves a dot-path color from the theme (e.g., `'text.primary'`), or returns the value as-is if not found.

### `getSpacing(theme, value)`

Resolves a spacing token (`'md'`), numeric factor (`4`), or returns the value as-is.

### `getThemeValue(obj, path)`

Generic dot-path accessor for theme objects.
