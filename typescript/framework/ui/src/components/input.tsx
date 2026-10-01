'use client';

import type { InputHTMLAttributes, ReactNode } from 'react';
import { forwardRef } from 'react';
import { css, styled } from '../emotion';

type InputSize = 'sm' | 'md' | 'lg';
type InputVariant = 'outline' | 'filled' | 'flushed';

/** Props for the Input component, a styled text input with outline, filled, and flushed variants. */
export interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'size'> {
  size?: InputSize;
  variant?: InputVariant;
  isInvalid?: boolean;
  isDisabled?: boolean;
  isReadOnly?: boolean;
  fullWidth?: boolean;
  leftElement?: ReactNode;
  rightElement?: ReactNode;
}

const sizeStyles: Record<InputSize, { height: string; padding: string; fontSize: string }> = {
  sm: { height: '32px', padding: '0 12px', fontSize: '0.875rem' },
  md: { height: '40px', padding: '0 16px', fontSize: '1rem' },
  lg: { height: '48px', padding: '0 20px', fontSize: '1.125rem' },
};

const InputWrapper = styled.div<{
  $size: InputSize;
  $variant: InputVariant;
  $fullWidth?: boolean;
  $isInvalid?: boolean;
  $isDisabled?: boolean;
  $hasLeftElement?: boolean;
  $hasRightElement?: boolean;
}>`
  display: inline-flex;
  align-items: center;
  position: relative;
  font-family: var(--font-sans);
  transition: all var(--transition-fast);

  ${({ $fullWidth }) =>
    $fullWidth
      ? css`
          width: 100%;
        `
      : css`
          width: auto;
        `}

  ${({ $size }) => css`
    height: ${sizeStyles[$size].height};
    font-size: ${sizeStyles[$size].fontSize};
  `}

  /* Variant: outline */
  ${({ $variant }) =>
    $variant === 'outline' &&
    css`
      background: transparent;
      border: 1px solid var(--color-border);
      border-radius: var(--radius-md);

      &:focus-within {
        border-color: var(--color-primary);
        box-shadow: 0 0 0 1px var(--color-primary);
      }
    `}

  /* Variant: filled */
  ${({ $variant }) =>
    $variant === 'filled' &&
    css`
      background: var(--color-surface);
      border: 1px solid transparent;
      border-radius: var(--radius-md);

      &:focus-within {
        background: var(--color-bg);
        border-color: var(--color-primary);
      }
    `}

  /* Variant: flushed */
  ${({ $variant }) =>
    $variant === 'flushed' &&
    css`
      background: transparent;
      border: none;
      border-bottom: 1px solid var(--color-border);
      border-radius: 0;

      &:focus-within {
        border-bottom-color: var(--color-primary);
        box-shadow: 0 1px 0 0 var(--color-primary);
      }
    `}

  /* Invalid state */
  ${({ $isInvalid, $variant }) =>
    $isInvalid &&
    css`
      border-color: var(--color-error) !important;

      &:focus-within {
        border-color: var(--color-error) !important;
        box-shadow: ${$variant === 'flushed' ? '0 1px 0 0 var(--color-error)' : '0 0 0 1px var(--color-error)'};
      }
    `}

  /* Disabled state */
  ${({ $isDisabled }) =>
    $isDisabled &&
    css`
      opacity: 0.5;
      cursor: not-allowed;
      background: var(--color-surface);
    `}
`;

const StyledInput = styled.input<{
  $size: InputSize;
  $hasLeftElement?: boolean;
  $hasRightElement?: boolean;
}>`
  flex: 1;
  width: 100%;
  height: 100%;
  background: transparent;
  border: none;
  outline: none;
  font-family: inherit;
  font-size: inherit;
  color: var(--color-text);

  ${({ $size, $hasLeftElement, $hasRightElement }) => {
    const padding = sizeStyles[$size].padding.split(' ')[1] || '16px';
    return css`
      padding-left: ${$hasLeftElement ? '0' : padding};
      padding-right: ${$hasRightElement ? '0' : padding};
    `;
  }}

  &::placeholder {
    color: var(--color-text-muted);
  }

  &:disabled {
    cursor: not-allowed;
  }
`;

const ElementWrapper = styled.span<{
  $position: 'left' | 'right';
  $size: InputSize;
}>`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  color: var(--color-text-muted);

  ${({ $position, $size }) => {
    const padding = sizeStyles[$size].padding.split(' ')[1] || '16px';
    return css`
      padding-left: ${$position === 'left' ? padding : '8px'};
      padding-right: ${$position === 'right' ? padding : '8px'};
    `;
  }}

  svg {
    width: 1em;
    height: 1em;
  }
`;

/** Renders a styled text input with optional left/right addon elements and validation state. */
export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  {
    size = 'md',
    variant = 'outline',
    isInvalid = false,
    isDisabled = false,
    isReadOnly = false,
    fullWidth = false,
    leftElement,
    rightElement,
    disabled,
    readOnly,
    ...props
  },
  ref,
) {
  const computedDisabled = isDisabled || disabled;
  const computedReadOnly = isReadOnly || readOnly;

  return (
    <InputWrapper
      $size={size}
      $variant={variant}
      $fullWidth={fullWidth}
      $isInvalid={isInvalid}
      $isDisabled={computedDisabled}
      $hasLeftElement={!!leftElement}
      $hasRightElement={!!rightElement}
    >
      {!!leftElement && (
        <ElementWrapper $position='left' $size={size}>
          {leftElement}
        </ElementWrapper>
      )}

      <StyledInput
        ref={ref}
        disabled={computedDisabled}
        readOnly={computedReadOnly}
        aria-invalid={isInvalid}
        $size={size}
        $hasLeftElement={!!leftElement}
        $hasRightElement={!!rightElement}
        {...props}
      />

      {!!rightElement && (
        <ElementWrapper $position='right' $size={size}>
          {rightElement}
        </ElementWrapper>
      )}
    </InputWrapper>
  );
});
