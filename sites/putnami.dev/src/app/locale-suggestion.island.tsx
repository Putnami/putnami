import { styled } from '@putnami/ui';
import { island, LocaleSuggestion } from '@putnami/web';
import suggestion from './locale-suggestion.messages';

/**
 * Offers the page in the reader's language, once the browser knows it: the
 * pages are pre-rendered and the same for every reader.
 */
function Suggestion() {
  return (
    <SuggestionBar>
      <LocaleSuggestion messages={suggestion} />
    </SuggestionBar>
  );
}

// One quiet line above the page; the link and the button read as text.
const SuggestionBar = styled.div`
  aside {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-md);
    margin: var(--space-md) 0;
    padding: var(--space-sm) var(--space-md);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-md);
    color: var(--color-text-muted);
    font-size: 0.875rem;
  }

  p {
    margin: 0;
  }

  a {
    color: var(--color-primary);
  }

  button {
    border: 0;
    background: none;
    color: var(--color-text-muted);
    font: inherit;
    cursor: pointer;
  }
`;

export default island().load('idle').render(Suggestion);
