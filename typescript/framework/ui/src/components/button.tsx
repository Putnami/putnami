'use client';

import type { ComponentProps, MouseEventHandler, ReactNode } from 'react';
import { forwardRef } from 'react';
import { css, styled } from '../emotion';
import type { BoxProps } from '../layout';
import type { SemanticColor, Theme } from '../theme/theme';
import { Spinner } from './spinner';

type ButtonVariant = 'solid' | 'outline' | 'ghost' | 'link';
type ButtonSize = 'sm' | 'md' | 'lg';
type ColorScheme = 'primary' | 'secondary' | 'error' | 'success' | 'warning' | 'info';

/** Props for the Button component, an interactive button with solid, outline, ghost, and link variants. */
export interface ButtonProps extends Omit<BoxProps, 'as' | 'color'> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  colorScheme?: ColorScheme;
  disabled?: boolean;
  loading?: boolean;
  fullWidth?: boolean;
  leftIcon?: ReactNode;
  rightIcon?: ReactNode;
  type?: 'button' | 'submit' | 'reset';
  onClick?: MouseEventHandler<HTMLButtonElement>;
  children?: ReactNode;
}

const sizeStyles: Record<ButtonSize, { height: string; padding: string; fontSize: string; gap: string }> = {
  sm: { height: '32px', padding: '0 12px', fontSize: '0.875rem', gap: '6px' },
  md: { height: '40px', padding: '0 16px', fontSize: '1rem', gap: '8px' },
  lg: { height: '48px', padding: '0 24px', fontSize: '1.125rem', gap: '10px' },
};

const getColorScheme = (theme: Theme, scheme: ColorScheme): SemanticColor => theme.colors[scheme];

const getVariantStyles = (theme: Theme, variant: ButtonVariant, colorScheme: ColorScheme) => {
  const color = getColorScheme(theme, colorScheme);

  switch (variant) {
    case 'solid':
      return css`
        background: ${color.main};
        color: ${color.contrastText};
        border: none;

        &:hover:not(:disabled) {
          background: ${color.dark};
        }

        &:active:not(:disabled) {
          background: ${color[700]};
        }
      `;

    case 'outline':
      return css`
        background: transparent;
        color: ${color.main};
        border: 1px solid ${color.main};

        &:hover:not(:disabled) {
          background: ${color[50]};
        }

        &:active:not(:disabled) {
          background: ${color[100]};
        }
      `;

    case 'ghost':
      return css`
        background: transparent;
        color: ${color.main};
        border: none;

        &:hover:not(:disabled) {
          background: ${color[50]};
        }

        &:active:not(:disabled) {
          background: ${color[100]};
        }
      `;

    case 'link':
      return css`
        background: transparent;
        color: ${color.main};
        border: none;
        padding: 0;
        height: auto;
        min-height: auto;

        &:hover:not(:disabled) {
          text-decoration: underline;
        }
      `;
  }
};

const StyledButton = styled.button<{
  $variant: ButtonVariant;
  $size: ButtonSize;
  $colorScheme: ColorScheme;
  $fullWidth?: boolean;
  $loading?: boolean;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-family: var(--font-sans);
  font-weight: 500;
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: all var(--transition-fast);
  text-decoration: none;
  white-space: nowrap;
  user-select: none;
  position: relative;

  /* Size styles */
  ${({ $size }) => css`
    height: ${sizeStyles[$size].height};
    min-height: ${sizeStyles[$size].height};
    padding: ${sizeStyles[$size].padding};
    font-size: ${sizeStyles[$size].fontSize};
    gap: ${sizeStyles[$size].gap};
  `}

  /* Variant styles */
  ${({ theme, $variant, $colorScheme }) => getVariantStyles(theme, $variant, $colorScheme)}

  /* Full width */
  ${({ $fullWidth }) =>
    $fullWidth &&
    css`
      width: 100%;
    `}

  /* Focus visible */
  &:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
  }

  /* Disabled state */
  &:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  /* Active press effect */
  &:active:not(:disabled) {
    transform: scale(0.98);
  }

  /* Loading state */
  ${({ $loading }) =>
    $loading &&
    css`
      cursor: wait;
      pointer-events: none;
    `}
`;

const IconWrapper = styled.span<{ $hidden?: boolean }>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;

  ${({ $hidden }) =>
    $hidden &&
    css`
      visibility: hidden;
    `}

  svg {
    width: 1em;
    height: 1em;
  }
`;

const ContentWrapper = styled.span<{ $hidden?: boolean }>`
  ${({ $hidden }) =>
    $hidden &&
    css`
      visibility: hidden;
    `}
`;

const LoadingWrapper = styled.span`
  position: absolute;
  display: inline-flex;
  align-items: center;
  justify-content: center;
`;

/** Renders an interactive button with configurable variant, size, color scheme, and loading state with spinner. */
export const Button = forwardRef<HTMLButtonElement, ButtonProps & ComponentProps<'button'>>(function Button(
  {
    variant = 'solid',
    size = 'md',
    colorScheme = 'primary',
    disabled = false,
    loading = false,
    fullWidth = false,
    leftIcon,
    rightIcon,
    type = 'button',
    onClick,
    children,
    ...props
  },
  ref,
) {
  const isDisabled = disabled || loading;

  return (
    <StyledButton
      ref={ref}
      type={type}
      disabled={isDisabled}
      onClick={onClick}
      $variant={variant}
      $size={size}
      $colorScheme={colorScheme}
      $fullWidth={fullWidth}
      $loading={loading}
      aria-busy={loading}
      aria-disabled={isDisabled}
      {...props}
    >
      {!!loading && (
        <LoadingWrapper>
          <Spinner size={size === 'lg' ? 'md' : 'sm'} />
        </LoadingWrapper>
      )}

      {!!leftIcon && <IconWrapper $hidden={loading}>{leftIcon}</IconWrapper>}
      <ContentWrapper $hidden={loading}>{children}</ContentWrapper>
      {!!rightIcon && <IconWrapper $hidden={loading}>{rightIcon}</IconWrapper>}
    </StyledButton>
  );
});
