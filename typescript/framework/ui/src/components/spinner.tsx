import type { ComponentPropsWithoutRef } from 'react';
import { forwardRef } from 'react';
import { css, keyframes, styled } from '../emotion';

type SpinnerSize = 'sm' | 'md' | 'lg';

/** Props for the Spinner component, an animated loading indicator. */
export interface SpinnerProps extends Omit<ComponentPropsWithoutRef<'span'>, 'color'> {
  size?: SpinnerSize;
  color?: string;
}

const spin = keyframes`
  0% {
    transform: rotate(0deg);
  }
  100% {
    transform: rotate(360deg);
  }
`;

const sizeMap: Record<SpinnerSize, string> = {
  sm: '14px',
  md: '18px',
  lg: '24px',
};

const borderWidthMap: Record<SpinnerSize, string> = {
  sm: '2px',
  md: '2px',
  lg: '3px',
};

const StyledSpinner = styled.span<{
  $size: SpinnerSize;
  $color?: string;
}>`
  display: inline-block;
  width: ${({ $size }) => sizeMap[$size]};
  height: ${({ $size }) => sizeMap[$size]};
  border-radius: 50%;
  border: ${({ $size }) => borderWidthMap[$size]} solid transparent;
  border-top-color: ${({ $color }) => $color || 'currentColor'};
  border-right-color: ${({ $color }) => $color || 'currentColor'};
  animation: ${spin} 0.6s linear infinite;

  ${({ $color }) =>
    !$color &&
    css`
      opacity: 0.8;
    `}
`;

/** Renders a circular spinning loading indicator. */
export const Spinner = forwardRef<HTMLSpanElement, SpinnerProps>(function Spinner(
  { size = 'md', color, ...rest },
  ref,
) {
  return <StyledSpinner ref={ref} $size={size} $color={color} role='status' aria-label='Loading' {...rest} />;
});
