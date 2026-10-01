'use client';

import { css, Global } from '../emotion';
import { useEffect } from 'react';
import { createTheme } from './default';
import { useTheme } from './hook';
import type { ColorPalette, Theme } from './theme';

const SCALE_STEPS = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900] as const;

/**
 * Emits `--<prefix>-50 … --<prefix>-900` custom properties for a color ramp.
 * Raw ramps are mode-independent: components should prefer the semantic tokens,
 * but the ramps are exposed for data viz (charts, heat maps) and the chart palette.
 */
function scaleVars(prefix: string, scale: ColorPalette): string {
  return SCALE_STEPS.map((step) => `--${prefix}-${step}: ${scale[step]};`).join('\n          ');
}

/** Emits semantic color properties for one resolved color palette. */
export function semanticColorVars(colors: Theme['colors']): string {
  return `
    --color-bg: ${colors.background};
    --color-surface: ${colors.surface};
    --color-surface-hover: ${colors.surfaceHover};
    --color-border: ${colors.border};
    --color-text: ${colors.text.primary};
    --color-text-muted: ${colors.text.secondary};
    --color-text-dim: ${colors.text.disabled};

    --color-primary: ${colors.primary.main};
    --color-primary-light: ${colors.primary.light};
    --color-primary-dark: ${colors.primary.dark};
    --color-on-primary: ${colors.primary.contrastText};
    --color-secondary: ${colors.secondary.main};
    --color-error: ${colors.error.main};
    --color-success: ${colors.success.main};
    --color-warning: ${colors.warning.main};
    --color-info: ${colors.info.main};
    --color-info-dark: ${colors.info.dark};`;
}

/** Props for the global-style palette overrides. */
export interface GlobalStylesProps {
  lightTheme?: Theme;
  darkTheme?: Theme;
}

/** Renders global CSS reset, typography, design tokens, and CSS custom properties for light/dark modes. */
export function GlobalStyles({
  lightTheme = createTheme('light'),
  darkTheme = createTheme('dark'),
}: GlobalStylesProps = {}) {
  const theme = useTheme<Theme>();
  const { fontSizes, fontWeights, lineHeights } = theme.typography;
  const term = theme.terminal;
  const lightColors = lightTheme.colors;
  const darkColors = darkTheme.colors;

  // Set data-color-mode on html element for CSS-based theme detection.
  // This drives all color switching via attribute selectors in the static CSS below.
  useEffect(() => {
    document.documentElement.dataset['colorMode'] = theme.colorMode;
    document.documentElement.style.colorScheme = theme.colorMode;
  }, [theme.colorMode]);

  return (
    <Global
      styles={css`
        /* ——— Light mode (default) ——— */
        :root {
          color-scheme: light;

          ${semanticColorVars(lightColors)}

          /* Raw color ramps (50–900) — mode-independent, for charts / data viz */
          ${scaleVars('teal', lightColors.primary)}
          ${scaleVars('neutral', lightColors.gray)}
          ${scaleVars('red', lightColors.error)}
          ${scaleVars('emerald', lightColors.success)}
          ${scaleVars('amber', lightColors.warning)}
          ${scaleVars('sky', lightColors.info)}

          /* Terminal palette — GitHub-dark, used only inside .surface-terminal */
          --term-bg: ${term.bg};
          --term-bg-2: ${term.bgAlt};
          --term-chrome: ${term.chrome};
          --term-fg: ${term.fg};
          --term-fg-muted: ${term.fgMuted};
          --term-prompt: ${term.prompt};
          --term-dir: ${term.dir};
          --term-git: ${term.git};
          --term-branch: ${term.branch};
          --term-dirty: ${term.dirty};
          --term-info: ${term.info};
          --term-warn: ${term.warn};

          /* Typography */
          --font-sans: ${theme.typography.fontFamily.sans};
          --font-mono: ${theme.typography.fontFamily.mono};

          /* Font sizes */
          --fs-xs: ${fontSizes.xs};
          --fs-sm: ${fontSizes.sm};
          --fs-md: ${fontSizes.md};
          --fs-lg: ${fontSizes.lg};
          --fs-xl: ${fontSizes.xl};
          --fs-2xl: ${fontSizes['2xl']};
          --fs-3xl: ${fontSizes['3xl']};
          --fs-4xl: ${fontSizes['4xl']};

          /* Font weights */
          --fw-regular: ${fontWeights.regular};
          --fw-medium: ${fontWeights.medium};
          --fw-semibold: ${fontWeights.semibold};
          --fw-bold: ${fontWeights.bold};

          /* Line heights */
          --lh-tight: ${lineHeights.tight};
          --lh-normal: ${lineHeights.normal};
          --lh-relaxed: ${lineHeights.relaxed};

          /* Spacing */
          --space-xs: ${theme.space.xs};
          --space-sm: ${theme.space.sm};
          --space-md: ${theme.space.md};
          --space-lg: ${theme.space.lg};
          --space-xl: ${theme.space.xl};
          --space-2xl: ${theme.space.xxl};
          --space-3xl: ${theme.spacing(16)};

          /* Border radius */
          --radius-sm: ${theme.radii.sm};
          --radius-md: ${theme.radii.md};
          --radius-lg: ${theme.radii.lg};
          --radius-xl: ${theme.radii.full};

          /* Shadows — neutral, low-contrast, no color tint */
          --shadow-sm: ${theme.shadows.sm};
          --shadow-md: ${theme.shadows.md};
          --shadow-lg: ${theme.shadows.lg};
          --shadow-xl: ${theme.shadows.xl};

          /* Transitions */
          --transition-fast: 150ms ease;
          --transition-base: 250ms ease;

          /* Breakpoints */
          --bp-sm: ${theme.breakpoints.sm}px;
          --bp-md: ${theme.breakpoints.md}px;
          --bp-lg: ${theme.breakpoints.lg}px;
          --bp-xl: ${theme.breakpoints.xl}px;
          --bp-xxl: ${theme.breakpoints.xxl}px;

          /* Container max-widths */
          --container-prose: 45rem; /* 720px — long-form article body */
          --container-hero: 47.5rem; /* 760px — hero / CTA bands */
          --container-grid: 56.25rem; /* 900px — section grids */
          --container-pricing: 62.5rem; /* 1000px — pricing tables */
          --container-app: 72rem; /* 1152px — marketing + console shell */
          --container-wide: 90rem; /* 1440px — full-bleed console + data */

          /* Icon sizes — Lucide stroke-width 2 across all sizes */
          --icon-xs: 12px;
          --icon-sm: 15px;
          --icon-md: 18px;
          --icon-lg: 20px;
          --icon-xl: 24px;

          /* Control density — height + horizontal padding + font-size */
          --control-h-sm: 32px;
          --control-h-md: 40px;
          --control-h-lg: 48px;
          --control-px-sm: 12px;
          --control-px-md: 16px;
          --control-px-lg: 24px;
          --control-fs-sm: var(--fs-sm);
          --control-fs-md: var(--fs-md);
          --control-fs-lg: var(--fs-lg);

          /* Minimum hit targets (touch + pointer) */
          --hit-target-min: 44px;
          --hit-target-pointer: 32px;

          /* Motion easings + overlay choreography */
          --ease-out: cubic-bezier(0.16, 1, 0.3, 1);
          --ease-in: cubic-bezier(0.7, 0, 0.84, 0);
          --ease-in-out: cubic-bezier(0.65, 0, 0.35, 1);
          --ease-linear: linear;
          --transition-slow: 350ms var(--ease-out);
          --motion-enter: 200ms var(--ease-out);
          --motion-exit: 150ms var(--ease-in);

          /* Syntax highlighting (GitHub-dark) — opt in with <pre class="code-block"> */
          --code-bg: var(--term-bg);
          --code-fg: var(--term-fg);
          --code-muted: var(--term-fg-muted);
          --code-line: #30363d;
          --tok-keyword: #ff7b72;
          --tok-string: #a5d6ff;
          --tok-number: #79c0ff;
          --tok-comment: #8b949e;
          --tok-function: #d2a8ff;
          --tok-variable: #ffa657;
          --tok-property: #79c0ff;
          --tok-operator: #ff7b72;
          --tok-tag: #7ee787;
          --tok-attr: #d2a8ff;
          --tok-punctuation: var(--term-fg);

          /* Chart palette — categorical, ordered for legibility (light mode) */
          --chart-1: var(--teal-500);
          --chart-2: var(--sky-500);
          --chart-3: #8b5cf6;
          --chart-4: var(--amber-500);
          --chart-5: var(--emerald-500);
          --chart-6: #ec4899;
          --chart-7: #6366f1;
          --chart-8: var(--neutral-500);
          --chart-pos: var(--emerald-500);
          --chart-neg: var(--red-500);
          --chart-grid: var(--color-border);
          --chart-axis: var(--color-text-dim);
        }

        /* ——— Dark mode overrides ——— */
        html[data-color-mode='dark'] {
          color-scheme: dark;

          ${semanticColorVars(darkColors)}

          /* Chart palette — dark mode (brighter hues for contrast) */
          --chart-1: var(--teal-400);
          --chart-2: var(--sky-400);
          --chart-3: #a78bfa;
          --chart-4: var(--amber-400);
          --chart-5: var(--emerald-400);
          --chart-6: #f472b6;
          --chart-7: #818cf8;
          --chart-8: var(--neutral-400);
          --chart-pos: var(--emerald-400);
          --chart-neg: var(--red-400);
        }

        /* Reset */
        *,
        *::before,
        *::after {
          box-sizing: border-box;
          margin: 0;
          padding: 0;
        }

        html {
          font-size: 16px;
          scroll-behavior: smooth;
          background-color: var(--color-bg);
          color: var(--color-text);
        }

        body {
          font-family: var(--font-sans);
          background: var(--color-bg);
          color: var(--color-text);
          line-height: 1.6;
          min-height: 100vh;
          overflow-x: hidden;
          -webkit-font-smoothing: antialiased;
          -moz-osx-font-smoothing: grayscale;
        }

        /*
         * Terminal surface — first-class, brand-defining.
         * Apply class="surface-terminal" to any container to adopt the CLI
         * aesthetic. It is mode-independent (always dark, GitHub-flavored) and
         * re-binds the semantic color tokens to the terminal palette so nested
         * components inherit the dark surface automatically.
         */
        .surface-terminal {
          --color-bg: var(--term-bg);
          --color-surface: var(--term-bg-2);
          --color-surface-hover: var(--term-chrome);
          --color-border: var(--term-chrome);
          --color-text: var(--term-fg);
          --color-text-muted: var(--term-fg-muted);
          --color-text-dim: var(--term-fg-muted);

          background: linear-gradient(135deg, var(--term-bg), var(--term-bg-2));
          color: var(--color-text);
          font-family: var(--font-mono);
        }

        /* Typography - Headings */
        h1,
        h2,
        h3,
        h4,
        h5,
        h6 {
          font-weight: ${theme.typography.headings.fontWeight};
          line-height: ${theme.typography.headings.lineHeight};
        }

        h1 {
          font-size: ${theme.typography.headings.h1.fontSize};
          margin-top: ${theme.typography.headings.h1.marginTop};
          margin-bottom: ${theme.typography.headings.h1.marginBottom};
        }

        h2 {
          font-size: ${theme.typography.headings.h2.fontSize};
          margin-top: ${theme.typography.headings.h2.marginTop};
          margin-bottom: ${theme.typography.headings.h2.marginBottom};
        }

        h3 {
          font-size: ${theme.typography.headings.h3.fontSize};
          margin-top: ${theme.typography.headings.h3.marginTop};
          margin-bottom: ${theme.typography.headings.h3.marginBottom};
        }

        h4 {
          font-size: ${theme.typography.headings.h4.fontSize};
          margin-top: ${theme.typography.headings.h4.marginTop};
          margin-bottom: ${theme.typography.headings.h4.marginBottom};
        }

        h5 {
          font-size: ${theme.typography.headings.h5.fontSize};
          margin-top: ${theme.typography.headings.h5.marginTop};
          margin-bottom: ${theme.typography.headings.h5.marginBottom};
        }

        h6 {
          font-size: ${theme.typography.headings.h6.fontSize};
          margin-top: ${theme.typography.headings.h6.marginTop};
          margin-bottom: ${theme.typography.headings.h6.marginBottom};
        }

        /* First heading shouldn't have top margin */
        h1:first-child,
        h2:first-child,
        h3:first-child,
        h4:first-child,
        h5:first-child,
        h6:first-child {
          margin-top: 0;
        }

        /* Typography - Paragraphs */
        p {
          margin-bottom: 1em;
        }

        p:last-child {
          margin-bottom: 0;
        }

        /*
         * Section label — the recurring "uppercase teal kicker" that opens
         * every marketing section. Sentence-case rules do not apply here.
         */
        .section-label {
          display: block;
          font-size: var(--fs-sm);
          font-weight: var(--fw-medium);
          color: var(--color-primary);
          text-transform: uppercase;
          letter-spacing: 0.05em;
          margin-bottom: var(--space-sm);
        }

        /* Display heading — hero use only. Responsive clamp matches the
           2xl→3xl→4xl step used by <Heading size={['2xl','3xl','4xl']}>. */
        .display {
          font-size: clamp(var(--fs-2xl), 2.4vw + 1rem, var(--fs-4xl));
          font-weight: var(--fw-bold);
          letter-spacing: -0.02em;
          line-height: var(--lh-tight);
          text-wrap: balance;
        }

        /*
         * Focus-visible policy. Mouse clicks never paint a ring; keyboard nav
         * always does. Inputs opt out here and supply their own inner ring.
         */
        :focus {
          outline: none;
        }

        :focus-visible {
          outline: 2px solid var(--color-primary);
          outline-offset: 2px;
          border-radius: var(--radius-sm);
        }

        input:focus-visible,
        textarea:focus-visible,
        select:focus-visible {
          outline: none;
        }

        /* Skip-link — visually hidden until focused. Place once at top of <body>. */
        .skip-link {
          position: absolute;
          left: var(--space-md);
          top: var(--space-md);
          padding: var(--space-sm) var(--space-md);
          background: var(--color-primary);
          color: var(--color-on-primary);
          border-radius: var(--radius-md);
          font-weight: var(--fw-medium);
          text-decoration: none;
          transform: translateY(-200%);
          transition: transform var(--motion-enter);
          z-index: 100;
        }

        .skip-link:focus-visible {
          transform: translateY(0);
        }

        /*
         * Syntax highlighting. Code is content for this brand. Opt in with
         * <pre class="code-block"> and wrap tokens in <span class="tok-*">.
         * For non-trivial docs, swap the tokenizer for Shiki/Prism under this
         * same palette.
         */
        pre.code-block {
          background: var(--code-bg);
          color: var(--code-fg);
          border: 1px solid var(--term-chrome);
          border-radius: var(--radius-md);
          padding: var(--space-md) var(--space-lg);
          font-family: var(--font-mono);
          font-size: var(--fs-sm);
          line-height: var(--lh-normal);
          overflow-x: auto;
        }

        pre.code-block .tok-keyword {
          color: var(--tok-keyword);
        }
        pre.code-block .tok-string {
          color: var(--tok-string);
        }
        pre.code-block .tok-number {
          color: var(--tok-number);
        }
        pre.code-block .tok-comment {
          color: var(--tok-comment);
          font-style: italic;
        }
        pre.code-block .tok-function {
          color: var(--tok-function);
        }
        pre.code-block .tok-variable {
          color: var(--tok-variable);
        }
        pre.code-block .tok-property {
          color: var(--tok-property);
        }
        pre.code-block .tok-operator {
          color: var(--tok-operator);
        }
        pre.code-block .tok-tag {
          color: var(--tok-tag);
        }
        pre.code-block .tok-attr {
          color: var(--tok-attr);
        }
        pre.code-block .tok-punctuation {
          color: var(--tok-punctuation);
        }

        /* Skeleton placeholders — a calm pulse, not a shimmer. */
        .skeleton {
          background: var(--color-surface-hover);
          border-radius: var(--radius-sm);
          animation: putnami-skeleton-pulse 1.4s var(--ease-in-out) infinite;
        }

        @keyframes putnami-skeleton-pulse {
          0%,
          100% {
            opacity: 1;
          }
          50% {
            opacity: 0.55;
          }
        }

        /* Shiki syntax highlighting theme switching */
        html[data-color-mode='light'] .shiki,
        html[data-color-mode='light'] .shiki span {
          color: var(--shiki-light) !important;
          background-color: var(--shiki-light-bg) !important;
        }

        html[data-color-mode='dark'] .shiki,
        html[data-color-mode='dark'] .shiki span {
          color: var(--shiki-dark) !important;
          background-color: var(--shiki-dark-bg) !important;
        }

        /*
         * Reduced motion. Collapses every transition/animation to near-zero.
         * The hero terminal typewriter — the brand's one animated artifact —
         * checks this media query in JS and renders its lines instantly.
         */
        @media (prefers-reduced-motion: reduce) {
          *,
          *::before,
          *::after {
            animation-duration: 0.001ms !important;
            animation-iteration-count: 1 !important;
            transition-duration: 0.001ms !important;
            scroll-behavior: auto !important;
          }
        }
      `}
    />
  );
}
