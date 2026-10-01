import type { Theme } from '../theme';

type ResponsiveValue<T> = T | T[]; // e.g. "16px" or ["8px","16px","24px"]

// order must match the breakpoints order you want
const breakpointOrder: (keyof Theme['breakpoints'])[] = ['sm', 'md', 'lg', 'xl', 'xxl'];

/** Generates responsive CSS with media queries for an array of breakpoint values, or a single static declaration for scalar values. */
export const responsiveCss = <T>(
  theme: Theme,
  property: string,
  value: ResponsiveValue<T>,
  transform?: (v: T) => string | number,
) => {
  const applyTransform = (v: T): string | number => (transform ? transform(v) : (v as unknown as string | number));

  if (!Array.isArray(value)) {
    return `${property}: ${applyTransform(value)};`;
  }

  const parts: string[] = [`${property}: ${applyTransform(value[0])};`];

  for (let i = 1; i < value.length; i++) {
    const bp = breakpointOrder[i - 1];
    if (!bp || value[i] == null) continue;
    parts.push(`@media (min-width: ${theme.breakpoints[bp]}px) { ${property}: ${applyTransform(value[i])}; }`);
  }

  return parts.join('\n');
};

/** Returns a min-width media query string for the given breakpoint. */
export const up = (theme: Theme, bp: keyof Theme['breakpoints']) => `@media (min-width: ${theme.breakpoints[bp]}px)`;

/** Returns a max-width media query string for the given breakpoint. */
export const down = (theme: Theme, bp: keyof Theme['breakpoints']) =>
  `@media (max-width: ${Number(theme.breakpoints[bp]) - 0.01}px)`;

/** Returns a media query string matching between two breakpoints. */
export const between = (theme: Theme, min: keyof Theme['breakpoints'], max: keyof Theme['breakpoints']) =>
  `@media (min-width: ${theme.breakpoints[min]}px) and (max-width: ${Number(theme.breakpoints[max]) - 0.01}px)`;
