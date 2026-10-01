import { Box, Flex } from '../layout';
import { CopyButton } from './copy-button';

/** Props for the CodeBlock component, a syntax-highlighted code block with optional copy button. */
export interface CodeBlockProps {
  code: string;
  language?: string;
  showCopy?: boolean;
}

/** Renders a styled code block with a language label header and an optional clipboard copy button. */
export function CodeBlock({ code, language = 'text', showCopy = true }: CodeBlockProps) {
  return (
    <Box
      bg='var(--color-surface)'
      style={{
        border: '1px solid var(--color-border)',
        borderRadius: 'var(--radius-lg)',
        overflow: 'hidden',
      }}
    >
      <Flex
        alignItems='center'
        justifyContent='space-between'
        gap='md'
        px='lg'
        py='md'
        bg='surfaceHover'
        style={{ borderBottom: '1px solid var(--color-border)' }}
      >
        <Box as='span' style={{ fontSize: '0.8rem', color: 'var(--color-text-dim)' }}>
          {language}
        </Box>
        {!!showCopy && <CopyButton text={code} />}
      </Flex>
      <Box
        as='pre'
        p='lg'
        style={{
          fontFamily: 'var(--font-mono)',
          fontSize: '0.9rem',
          lineHeight: 1.8,
          overflowX: 'auto',
        }}
      >
        <code className={`language-${language}`} style={{ fontFamily: 'inherit' }}>
          {code}
        </code>
      </Box>
    </Box>
  );
}
