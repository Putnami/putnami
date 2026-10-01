import { styled } from '@putnami/ui';
import { island } from '@putnami/web';

/**
 * The docs-hub search field. A static page ships zero JS, so this prominent
 * "search across every tool" trigger hydrates as its own small island: clicking
 * it dispatches the same synthetic ⌘K keydown the navbar uses, which the search
 * island's document listener picks up across island boundaries and opens the
 * command palette.
 */
function HubSearch() {
  const openPalette = () => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true }));
  return (
    <Trigger type='button' onClick={openPalette} aria-label='Search the documentation'>
      <SearchIcon />
      <span className='label'>Search across every tool…</span>
      <span className='kbd'>
        <kbd>⌘</kbd>
        <kbd>K</kbd>
      </span>
    </Trigger>
  );
}

function SearchIcon() {
  return (
    <svg
      width='19'
      height='19'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <circle cx='11' cy='11' r='8' />
      <line x1='21' y1='21' x2='16.65' y2='16.65' />
    </svg>
  );
}

const Trigger = styled.button`
  width: 100%;
  max-width: 560px;
  display: flex;
  align-items: center;
  gap: 12px;
  height: 52px;
  padding: 0 18px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  cursor: pointer;
  color: var(--color-text-dim);
  font-family: var(--font-sans);
  transition: all var(--transition-fast);
  text-align: left;

  &:hover {
    border-color: var(--color-text-dim);
  }

  .label {
    flex: 1;
    font-size: 0.95rem;
    color: var(--color-text-muted);
  }

  .kbd {
    display: flex;
    gap: 3px;
  }

  kbd {
    font-family: var(--font-sans);
    font-size: 0.75rem;
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    border-radius: 4px;
    padding: 2px 7px;
    color: var(--color-text-muted);
  }
`;

export default island().load('idle').render(HubSearch);
