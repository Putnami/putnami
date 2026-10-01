import type { ComponentProps } from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';
import { responsiveCss } from '../layout/responsive';
import type { Theme } from '../theme/theme';

type TextSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | '2xl' | '3xl' | '4xl';
type TextWeight = 'regular' | 'medium' | 'bold';
type TextColor = 'primary' | 'secondary' | 'disabled' | 'error' | 'success' | 'warning' | 'info';
type TextAlign = 'left' | 'center' | 'right' | 'justify';

/** Props for the Text component, a styled text span with configurable font size, weight, color, and line clamp. */
export interface TextProps extends Omit<BoxProps, 'color'> {
  as?: 'p' | 'span' | 'div' | 'label' | 'small' | 'strong' | 'em';
  size?: TextSize | TextSize[];
  weight?: TextWeight;
  color?: TextColor | string;
  align?: TextAlign | TextAlign[];
  truncate?: boolean;
  lineClamp?: number;
}

/*
 * Colors resolve to the custom properties `GlobalStyles` emits, never to
 * `theme.colors.*`. Emotion hashes the declaration into the class name once, at
 * SSR time, where the palette is always light — a baked literal would stay light
 * forever, while a `var()` reference is re-resolved by the browser on every
 * `html[data-color-mode]` flip.
 */
const textColorVars: Record<string, string> = {
  primary: 'var(--color-text)',
  secondary: 'var(--color-text-muted)',
  disabled: 'var(--color-text-dim)',
};

const semanticColorVars: Record<string, string> = {
  error: 'var(--color-error)',
  success: 'var(--color-success)',
  warning: 'var(--color-warning)',
  info: 'var(--color-info)',
};

const getTextColor = (color: TextProps['color']): string => {
  if (!color) return 'inherit';

  // Check for text semantic colors
  if (color in textColorVars) {
    return textColorVars[color];
  }

  // Check for semantic colors (error, success, warning, info)
  if (color in semanticColorVars) {
    return semanticColorVars[color];
  }

  // Return as-is (custom color)
  return color;
};

const getFontSize = (theme: Theme, size: TextSize): string => theme.typography.fontSizes[size] || size;

const getFontWeight = (theme: Theme, weight: TextWeight): number =>
  theme.typography.fontWeights[weight] || theme.typography.fontWeights.regular;

const StyledText = styled(Box)<{
  $size?: TextSize | TextSize[];
  $weight?: TextWeight;
  $color?: TextProps['color'];
  $align?: TextAlign | TextAlign[];
  $truncate?: boolean;
  $lineClamp?: number;
}>`
  font-family: var(--font-sans);
  line-height: ${({ theme }) => theme.typography.lineHeights.normal};

  ${({ theme, $size }) => $size && responsiveCss(theme, 'font-size', $size, (v) => getFontSize(theme, v as TextSize))}

  ${({ theme, $weight }) =>
    $weight &&
    css`
      font-weight: ${getFontWeight(theme, $weight)};
    `}

  ${({ $color }) =>
    $color &&
    css`
      color: ${getTextColor($color)};
    `}

  ${({ theme, $align }) => $align && responsiveCss(theme, 'text-align', $align)}

  ${({ $truncate }) =>
    $truncate &&
    css`
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    `}

  ${({ $lineClamp }) =>
    $lineClamp &&
    css`
      display: -webkit-box;
      -webkit-line-clamp: ${$lineClamp};
      -webkit-box-orient: vertical;
      overflow: hidden;
    `}
`;

/** Renders a styled text element with responsive size, weight, color, and truncation support. */
export function Text({
  as = 'span',
  size = 'md',
  weight,
  color,
  align,
  truncate,
  lineClamp,
  ...props
}: TextProps & ComponentProps<typeof Box>) {
  return (
    <StyledText
      as={as}
      $size={size}
      $weight={weight}
      $color={color}
      $align={align}
      $truncate={truncate}
      $lineClamp={lineClamp}
      {...props}
    />
  );
}
