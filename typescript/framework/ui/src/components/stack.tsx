import type { ComponentProps, ReactNode } from 'react';
import { css, styled } from '../emotion';
import { Box, type BoxProps } from '../layout';
import { responsiveCss } from '../layout/responsive';
import type { Theme } from '../theme/theme';

type StackDirection = 'row' | 'column' | 'row-reverse' | 'column-reverse';
type StackAlign = 'start' | 'center' | 'end' | 'stretch' | 'baseline';
type StackJustify = 'start' | 'center' | 'end' | 'between' | 'around' | 'evenly';
type StackSpacing = 'xs' | 'sm' | 'md' | 'lg' | 'xl' | 'xxl' | number;

/** Props for the Stack component, a flexbox layout with configurable direction, spacing, and alignment. */
export interface StackProps extends BoxProps {
  direction?: StackDirection | StackDirection[];
  align?: StackAlign;
  justify?: StackJustify;
  spacing?: StackSpacing | StackSpacing[];
  wrap?: boolean;
  divider?: ReactNode;
  children?: ReactNode;
}

const alignMap: Record<StackAlign, string> = {
  start: 'flex-start',
  center: 'center',
  end: 'flex-end',
  stretch: 'stretch',
  baseline: 'baseline',
};

const justifyMap: Record<StackJustify, string> = {
  start: 'flex-start',
  center: 'center',
  end: 'flex-end',
  between: 'space-between',
  around: 'space-around',
  evenly: 'space-evenly',
};

const getSpacing = (spacing: StackSpacing, theme: Theme): string => {
  if (typeof spacing === 'number') {
    return theme.spacing(spacing);
  }
  return theme.space[spacing] || spacing;
};

const StyledStack = styled(Box)<{
  $direction?: StackDirection | StackDirection[];
  $align?: StackAlign;
  $justify?: StackJustify;
  $spacing?: StackSpacing | StackSpacing[];
  $wrap?: boolean;
}>`
  display: flex;

  ${({ theme, $direction }) => $direction && responsiveCss(theme, 'flex-direction', $direction)}

  ${({ $align }) =>
    $align &&
    css`
      align-items: ${alignMap[$align]};
    `}

  ${({ $justify }) =>
    $justify &&
    css`
      justify-content: ${justifyMap[$justify]};
    `}

  ${({ theme, $spacing }) =>
    $spacing && responsiveCss(theme, 'gap', $spacing, (v) => getSpacing(v as StackSpacing, theme))}

  ${({ $wrap }) =>
    $wrap &&
    css`
      flex-wrap: wrap;
    `}
`;

/** Renders a vertical or horizontal stack of elements with configurable gap and alignment. */
export function Stack({
  direction = 'column',
  align,
  justify,
  spacing = 'md',
  wrap = false,
  children,
  ...props
}: StackProps & ComponentProps<typeof Box>) {
  return (
    <StyledStack $direction={direction} $align={align} $justify={justify} $spacing={spacing} $wrap={wrap} {...props}>
      {children}
    </StyledStack>
  );
}

// Convenience components
/** Renders a horizontal stack (row direction) with centered vertical alignment. */
export function HStack(props: Omit<StackProps, 'direction'> & ComponentProps<typeof Box>) {
  return <Stack direction='row' align='center' {...props} />;
}

/** Renders a vertical stack (column direction). */
export function VStack(props: Omit<StackProps, 'direction'> & ComponentProps<typeof Box>) {
  return <Stack direction='column' {...props} />;
}
