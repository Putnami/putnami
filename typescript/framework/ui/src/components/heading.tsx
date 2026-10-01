import type { ComponentProps } from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';
import { responsiveCss } from '../layout/responsive';
import type { Theme } from '../theme/theme';

type HeadingLevel = 1 | 2 | 3 | 4 | 5 | 6;
type HeadingElement = 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
type HeadingSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | '2xl' | '3xl' | '4xl';
type HeadingWeight = 'regular' | 'medium' | 'bold';
type HeadingAlign = 'left' | 'center' | 'right';

/** Props for the Heading component, a themed heading element (h1-h6) with configurable size and weight. */
export interface HeadingProps extends Omit<BoxProps, 'color'> {
  as?: HeadingElement;
  level?: HeadingLevel;
  size?: HeadingSize | HeadingSize[];
  weight?: HeadingWeight;
  color?: 'primary' | 'secondary' | 'disabled' | string;
  align?: HeadingAlign | HeadingAlign[];
  truncate?: boolean;
}

const levelToSize: Record<HeadingLevel, HeadingSize> = {
  1: '3xl',
  2: '2xl',
  3: 'xl',
  4: 'lg',
  5: 'md',
  6: 'sm',
};

const levelToElement: Record<HeadingLevel, HeadingElement> = {
  1: 'h1',
  2: 'h2',
  3: 'h3',
  4: 'h4',
  5: 'h5',
  6: 'h6',
};

/*
 * Text colors resolve to the custom properties `GlobalStyles` emits, never to
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

const getHeadingColor = (color: HeadingProps['color']): string => {
  if (!color) return 'var(--color-text)';

  return textColorVars[color] || color;
};

const getFontSize = (theme: Theme, size: HeadingSize): string => theme.typography.fontSizes[size] || size;

const getFontWeight = (theme: Theme, weight: HeadingWeight): number =>
  theme.typography.fontWeights[weight] || theme.typography.fontWeights.bold;

const StyledHeading = styled(Box)<{
  $size?: HeadingSize | HeadingSize[];
  $weight?: HeadingWeight;
  $color?: HeadingProps['color'];
  $align?: HeadingAlign | HeadingAlign[];
  $truncate?: boolean;
}>`
  font-family: var(--font-sans);

  ${({ theme, $size }) =>
    $size && responsiveCss(theme, 'font-size', $size, (v) => getFontSize(theme, v as HeadingSize))}

  ${({ theme, $weight }) =>
    css`
      font-weight: ${getFontWeight(theme, $weight || 'bold')};
    `}

  ${({ $color }) =>
    css`
      color: ${getHeadingColor($color)};
    `}

  ${({ theme, $align }) => $align && responsiveCss(theme, 'text-align', $align)}

  ${({ $truncate }) =>
    $truncate &&
    css`
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    `}
`;

/** Renders a themed heading element with automatic size mapping from heading level. */
export function Heading({
  as,
  level = 2,
  size,
  weight = 'bold',
  color,
  align,
  truncate,
  ...props
}: HeadingProps & ComponentProps<typeof Box>) {
  const element = as || levelToElement[level];
  const computedSize = size || levelToSize[level];

  return (
    <StyledHeading
      as={element}
      $size={computedSize}
      $weight={weight}
      $color={color}
      $align={align}
      $truncate={truncate}
      {...props}
    />
  );
}
