'use client';

import { css, keyframes, styled } from '../emotion';
import type { Theme } from '../theme/theme';

type ProgressSize = 'sm' | 'md' | 'lg';
type ProgressColorScheme = 'primary' | 'secondary' | 'success' | 'warning' | 'error' | 'info';

/** Props for the Progress component, a horizontal progress bar with animated fill. */
export interface ProgressProps {
  value?: number;
  max?: number;
  size?: ProgressSize;
  colorScheme?: ProgressColorScheme;
  isIndeterminate?: boolean;
  hasStripe?: boolean;
  isAnimated?: boolean;
  className?: string;
  'aria-label'?: string;
}

const sizeStyles: Record<ProgressSize, string> = {
  sm: '4px',
  md: '8px',
  lg: '12px',
};

const indeterminateAnimation = keyframes`
  0% {
    left: -40%;
  }
  100% {
    left: 100%;
  }
`;

const stripeAnimation = keyframes`
  0% {
    background-position: 1rem 0;
  }
  100% {
    background-position: 0 0;
  }
`;

const getColorScheme = (theme: Theme, scheme: ProgressColorScheme) => theme.colors[scheme];

const ProgressTrack = styled.div<{
  $size: ProgressSize;
}>`
  width: 100%;
  background: var(--color-surface-hover);
  border-radius: var(--radius-full);
  overflow: hidden;
  position: relative;

  ${({ $size }) => css`
    height: ${sizeStyles[$size]};
  `}
`;

const ProgressBar = styled.div<{
  $value: number;
  $colorScheme: ProgressColorScheme;
  $isIndeterminate?: boolean;
  $hasStripe?: boolean;
  $isAnimated?: boolean;
}>`
  height: 100%;
  border-radius: inherit;
  transition: width 0.3s ease;

  ${({ theme, $colorScheme }) => css`
    background: ${getColorScheme(theme, $colorScheme).main};
  `}

  ${({ $value, $isIndeterminate }) =>
    $isIndeterminate
      ? css`
          position: absolute;
          width: 40%;
          animation: ${indeterminateAnimation} 1.2s ease-in-out infinite;
        `
      : css`
          width: ${Math.min(100, Math.max(0, $value))}%;
        `}

  ${({ $hasStripe }) =>
    $hasStripe &&
    css`
      background-image: linear-gradient(
        45deg,
        rgba(255, 255, 255, 0.15) 25%,
        transparent 25%,
        transparent 50%,
        rgba(255, 255, 255, 0.15) 50%,
        rgba(255, 255, 255, 0.15) 75%,
        transparent 75%,
        transparent
      );
      background-size: 1rem 1rem;
    `}

  ${({ $hasStripe, $isAnimated, $isIndeterminate }) =>
    $hasStripe &&
    $isAnimated &&
    !$isIndeterminate &&
    css`
      animation: ${stripeAnimation} 1s linear infinite;
    `}
`;

/** Renders a horizontal progress bar with optional stripe pattern and indeterminate animation. */
export function Progress({
  value = 0,
  max = 100,
  size = 'md',
  colorScheme = 'primary',
  isIndeterminate = false,
  hasStripe = false,
  isAnimated = false,
  className,
  'aria-label': ariaLabel,
}: ProgressProps) {
  const percentage = (value / max) * 100;

  return (
    <ProgressTrack
      $size={size}
      className={className}
      role='progressbar'
      aria-valuenow={isIndeterminate ? undefined : Math.round(percentage)}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-label={ariaLabel}
    >
      <ProgressBar
        $value={percentage}
        $colorScheme={colorScheme}
        $isIndeterminate={isIndeterminate}
        $hasStripe={hasStripe}
        $isAnimated={isAnimated}
      />
    </ProgressTrack>
  );
}
