'use client';

import { useState } from 'react';
import { styled } from '../emotion';
import { Box } from '../layout';
import { useTheme } from '../theme';
import type { Theme } from '../theme/theme';

/** Props for the CopyButton component, a button that copies text to the clipboard with visual feedback. */
export interface CopyButtonProps {
  text: string;
  className?: string;
}

const StyledButton = styled(Box)`
  cursor: pointer;
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover) !important;
    color: var(--color-text) !important;
  }
`;

type CopyState = 'idle' | 'copied' | 'error';

/** Renders a button that copies the given text to the clipboard and shows a success or error icon. */
export function CopyButton({ text, className = '' }: CopyButtonProps) {
  const [state, setState] = useState<CopyState>('idle');
  const theme = useTheme<Theme>();

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setState('copied');
      setTimeout(() => setState('idle'), 2000);
    } catch (_err) {
      setState('error');
      setTimeout(() => setState('idle'), 2000);
    }
  };

  const label = state === 'copied' ? 'Copied' : state === 'error' ? 'Failed to copy' : 'Copy to clipboard';

  return (
    <StyledButton
      as='button'
      className={className}
      onClick={handleCopy}
      aria-label={label}
      title={label}
      type='button'
      display='flex'
      alignItems='center'
      justifyContent='center'
      width='40px'
      height='40px'
      bg='surface'
      style={{
        border: '1px solid var(--color-border)',
        borderRadius: 'var(--radius-md)',
        color: 'var(--color-text-muted)',
      }}
    >
      {state === 'copied' ? (
        <Box
          as='svg'
          width='18px'
          height='18px'
          viewBox='0 0 24 24'
          fill='none'
          style={{ stroke: 'currentColor', strokeWidth: '2', color: theme.colors.success.main }}
        >
          <title>Copied</title>
          <polyline points='20 6 9 17 4 12' />
        </Box>
      ) : state === 'error' ? (
        <Box
          as='svg'
          width='18px'
          height='18px'
          viewBox='0 0 24 24'
          fill='none'
          style={{ stroke: 'currentColor', strokeWidth: '2', color: theme.colors.error.main }}
        >
          <title>Failed to copy</title>
          <circle cx='12' cy='12' r='10' />
          <line x1='15' y1='9' x2='9' y2='15' />
          <line x1='9' y1='9' x2='15' y2='15' />
        </Box>
      ) : (
        <Box
          as='svg'
          width='18px'
          height='18px'
          viewBox='0 0 24 24'
          fill='none'
          style={{ stroke: 'currentColor', strokeWidth: '2' }}
          aria-label='Copy to clipboard'
        >
          <title>Copy to clipboard</title>
          <rect x='9' y='9' width='13' height='13' rx='2' ry='2' />
          <path d='M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1' />
        </Box>
      )}
    </StyledButton>
  );
}
