import type { ComponentPropsWithoutRef, ReactNode } from 'react';
import { forwardRef } from 'react';
import { css, styled } from '../emotion';
import type { Theme } from '../theme/theme';

type BadgeVariant = 'solid' | 'subtle' | 'outline';
type BadgeSize = 'sm' | 'md' | 'lg';
type BadgeColorScheme = 'gray' | 'primary' | 'secondary' | 'error' | 'success' | 'warning' | 'info';

/** Props for the Badge component, a small label for status or category with color scheme and variant support. */
export interface BadgeProps extends ComponentPropsWithoutRef<'span'> {
  variant?: BadgeVariant;
  size?: BadgeSize;
  colorScheme?: BadgeColorScheme;
  children?: ReactNode;
}

const sizeStyles: Record<BadgeSize, { fontSize: string; padding: string; borderRadius: string }> = {
  sm: { fontSize: '0.65rem', padding: '2px 6px', borderRadius: 'var(--radius-sm)' },
  md: { fontSize: '0.75rem', padding: '2px 8px', borderRadius: 'var(--radius-sm)' },
  lg: { fontSize: '0.85rem', padding: '4px 10px', borderRadius: 'var(--radius-md)' },
};

const getColorScheme = (theme: Theme, scheme: BadgeColorScheme) => {
  if (scheme === 'gray') {
    return {
      main: theme.colors.gray[500],
      light: theme.colors.gray[100],
      dark: theme.colors.gray[700],
      50: theme.colors.gray[50],
      100: theme.colors.gray[100],
      600: theme.colors.gray[600],
      contrastText: theme.colors.white,
    };
  }
  return theme.colors[scheme];
};

const StyledBadge = styled.span<{
  $variant: BadgeVariant;
  $size: BadgeSize;
  $colorScheme: BadgeColorScheme;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-family: var(--font-sans);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.02em;
  white-space: nowrap;
  vertical-align: middle;

  ${({ $size }) => css`
    font-size: ${sizeStyles[$size].fontSize};
    padding: ${sizeStyles[$size].padding};
    border-radius: ${sizeStyles[$size].borderRadius};
  `}

  /* Variant: solid */
  ${({ theme, $colorScheme, $variant }) =>
    $variant === 'solid' &&
    css`
      background: ${getColorScheme(theme, $colorScheme).main};
      color: ${getColorScheme(theme, $colorScheme).contrastText};
    `}

  /* Variant: subtle */
  ${({ theme, $colorScheme, $variant }) =>
    $variant === 'subtle' &&
    css`
      background: ${getColorScheme(theme, $colorScheme)[100]};
      color: ${getColorScheme(theme, $colorScheme)[600]};
    `}

  /* Variant: outline */
  ${({ theme, $colorScheme, $variant }) =>
    $variant === 'outline' &&
    css`
      background: transparent;
      color: ${getColorScheme(theme, $colorScheme).main};
      box-shadow: inset 0 0 0 1px ${getColorScheme(theme, $colorScheme).main};
    `}
`;

/** Renders a small inline label with solid, subtle, or outline styling. */
export const Badge = forwardRef<HTMLSpanElement, BadgeProps>(function Badge(
  { variant = 'subtle', size = 'md', colorScheme = 'gray', children, ...rest },
  ref,
) {
  return (
    <StyledBadge ref={ref} $variant={variant} $size={size} $colorScheme={colorScheme} {...rest}>
      {children}
    </StyledBadge>
  );
});
