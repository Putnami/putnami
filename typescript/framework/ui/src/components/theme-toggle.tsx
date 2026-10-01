'use client';

import { keyframes, styled } from '../emotion';
import { useColorMode } from '../theme/hook';
import type { ColorMode } from '../theme/theme';

const spin = keyframes`
  from {
    transform: rotate(0deg);
  }
  to {
    transform: rotate(360deg);
  }
`;

const ToggleButton = styled.button`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 0.5rem;
  border: none;
  background: transparent;
  color: var(--color-text-muted);
  cursor: pointer;
  border-radius: var(--radius-md);
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
    color: var(--color-text);
  }

  &:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
  }

  svg {
    width: 20px;
    height: 20px;
    transition: transform 0.3s ease;
  }

  &:hover svg {
    animation: ${spin} 0.5s ease-in-out;
  }
`;

// Sun icon for light mode
function SunIcon() {
  return (
    <svg
      xmlns='http://www.w3.org/2000/svg'
      fill='none'
      viewBox='0 0 24 24'
      strokeWidth={1.5}
      stroke='currentColor'
      aria-hidden='true'
    >
      <path
        strokeLinecap='round'
        strokeLinejoin='round'
        d='M12 3v2.25m6.364.386-1.591 1.591M21 12h-2.25m-.386 6.364-1.591-1.591M12 18.75V21m-4.773-4.227-1.591 1.591M5.25 12H3m4.227-4.773L5.636 5.636M15.75 12a3.75 3.75 0 1 1-7.5 0 3.75 3.75 0 0 1 7.5 0Z'
      />
    </svg>
  );
}

// Moon icon for dark mode
function MoonIcon() {
  return (
    <svg
      xmlns='http://www.w3.org/2000/svg'
      fill='none'
      viewBox='0 0 24 24'
      strokeWidth={1.5}
      stroke='currentColor'
      aria-hidden='true'
    >
      <path
        strokeLinecap='round'
        strokeLinejoin='round'
        d='M21.752 15.002A9.72 9.72 0 0 1 18 15.75c-5.385 0-9.75-4.365-9.75-9.75 0-1.33.266-2.597.748-3.752A9.753 9.753 0 0 0 3 11.25C3 16.635 7.365 21 12.75 21a9.753 9.753 0 0 0 9.002-5.998Z'
      />
    </svg>
  );
}

/** Props for the ThemeToggle component, a button that cycles through light, dark, and system color modes. */
export interface ThemeToggleProps {
  /** Optional class name. */
  className?: string;
  /** Optional inline styles. */
  style?: React.CSSProperties;
}

/**
 * Theme toggle button component.
 * Switches between light, dark, and system color modes.
 */
export function ThemeToggle({ className, style }: ThemeToggleProps) {
  const { colorMode, setColorMode, resolvedColorMode } = useColorMode();

  const handleClick = () => {
    // Cycle through: light -> dark -> system -> light
    const nextMode: ColorMode = colorMode === 'light' ? 'dark' : colorMode === 'dark' ? 'system' : 'light';
    setColorMode(nextMode);
  };

  const label =
    colorMode === 'system' ? `System theme (${resolvedColorMode})` : colorMode === 'dark' ? 'Dark mode' : 'Light mode';

  return (
    <ToggleButton
      type='button'
      onClick={handleClick}
      className={className}
      style={style}
      aria-label={`Toggle theme. Currently: ${label}`}
      title={label}
    >
      {resolvedColorMode === 'dark' ? <MoonIcon /> : <SunIcon />}
    </ToggleButton>
  );
}
