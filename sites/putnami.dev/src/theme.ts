import type { Theme } from '@putnami/ui';

/** Content shell width, shared by the navbar and the page container so they stay aligned. */
export const SITE_MAX_WIDTH = '1400px';

/**
 * Sticky offset for the docs rails (left nav, on-this-page TOC): the fixed
 * 64px navbar plus 24px of breathing room. Shared so both rails pin at the
 * same height.
 */
export const DOCS_STICKY_TOP = 88;

export const putnamiTheme: Partial<Theme> = {
  typography: {
    fontFamily: {
      sans: '"Inter", system-ui, sans-serif',
      mono: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
    },
  },
} as Partial<Theme>;
