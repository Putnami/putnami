import { styled } from '@putnami/ui';
import { island } from '@putnami/web';
import { useState } from 'react';
import { copyPageAsMarkdown } from '../../../lib/page-markdown';

/**
 * The quiet "Copy as Markdown" agent affordance on doc pages. Static pages ship
 * zero JS, so this hydrates as its own small island: it reads the static doc
 * body (`[data-markdown-root]`) and copies an LLM-friendly payload. The button
 * is a usable, labelled control whether or not JS has loaded.
 */
function CopyMarkdown() {
  const [done, setDone] = useState(false);

  const onClick = async () => {
    const ok = await copyPageAsMarkdown();
    if (ok) {
      setDone(true);
      setTimeout(() => setDone(false), 1600);
    }
  };

  return (
    <CopyButton
      type='button'
      onClick={onClick}
      $done={done}
      title='Copy this page as Markdown for an LLM or agent'
      // Declared in src/analytics.ts. One capturing listener on the document
      // reads these attributes, so the click needs no analytics import.
      data-track='doc_copy_markdown'
      data-track-surface='page'
    >
      {done ? <CheckIcon /> : <ClipboardIcon />}
      {done ? 'Copied for LLM' : 'Copy as Markdown'}
    </CopyButton>
  );
}

function ClipboardIcon() {
  return (
    <svg
      width='14'
      height='14'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <rect width='8' height='4' x='8' y='2' rx='1' ry='1' />
      <path d='M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2' />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg
      width='14'
      height='14'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2.5'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M20 6 9 17l-5-5' />
    </svg>
  );
}

const CopyButton = styled.button<{ $done: boolean }>`
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 32px;
  padding: 0 11px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  cursor: pointer;
  color: ${({ $done }) => ($done ? 'var(--color-success)' : 'var(--color-text-muted)')};
  font-size: 0.78rem;
  font-weight: 500;
  font-family: var(--font-sans);
  white-space: nowrap;
  transition: all var(--transition-fast);

  &:hover {
    border-color: var(--color-text-dim);
    color: ${({ $done }) => ($done ? 'var(--color-success)' : 'var(--color-text)')};
  }
`;

export default island().load('idle').render(CopyMarkdown);
