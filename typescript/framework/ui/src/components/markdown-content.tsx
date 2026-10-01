import { styled } from '../emotion';

/**
 * Styled container that applies typography and layout styles to rendered markdown HTML content.
 *
 * This is a plain styled `div`: it does not sanitize its content. When injecting an HTML
 * string with `dangerouslySetInnerHTML`, pass it through {@link sanitizeHtml} first (or use
 * {@link MarkdownRenderer}, which sanitizes automatically). Never inject unsanitized,
 * user-controlled HTML.
 */
export const MarkdownContent = styled.div`
  line-height: 1.8;

  h1,
  h2,
  h3,
  h4,
  h5,
  h6 {
    margin-top: var(--space-2xl);
    margin-bottom: var(--space-md);
    font-weight: 700;
    line-height: 1.3;
  }

  h1 {
    font-size: 2rem;
    border-bottom: 1px solid var(--color-border);
    padding-bottom: var(--space-sm);
  }

  h2 {
    font-size: 1.5rem;
    margin-top: var(--space-xl);
  }

  h3 {
    font-size: 1.25rem;
  }

  p {
    margin-bottom: var(--space-md);
    color: var(--color-text);
  }

  ul,
  ol {
    margin-bottom: var(--space-md);
    padding-left: var(--space-xl);
  }

  li {
    margin-bottom: var(--space-xs);
    color: var(--color-text);
  }

  code {
    font-family: var(--font-mono);
    font-size: 0.9em;
    background: var(--color-surface-hover);
    padding: 0.2em 0.4em;
    border-radius: var(--radius-sm);
    border: 1px solid var(--color-border);
    color: var(--color-text);
  }

    pre {
      border: 1px solid var(--color-border);
      border-radius: var(--radius-lg);
      padding: var(--space-lg);
      overflow-x: auto;
      max-width: 100%;
      margin-bottom: var(--space-md);

      code {
        background: transparent;
        padding: 0;
        border: none;
        font-size: 0.9rem;
        /* Inherit the pre foreground (the light --code-fg on the dark
           terminal-styled pre.code-block, or the shiki theme color) instead of
           the inline-code --color-text, which is dark in light mode and would
           render dark-on-dark inside a dark code block. */
        color: inherit;
      }
    }

    /* Shiki code blocks */
    pre.shiki {
      border-radius: var(--radius-lg);
    }

    .code-wrapper {
      position: relative;
      margin-bottom: var(--space-md);

      pre {
        margin-bottom: 0;
      }

      .copy-button {
        position: absolute;
        top: var(--space-sm);
        right: var(--space-sm);
        display: flex;
        align-items: center;
        justify-content: center;
        width: 32px;
        height: 32px;
        padding: 0;
        color: var(--color-text-muted);
        background: var(--color-bg);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        cursor: pointer;
        opacity: 0;
        transition: all var(--transition-fast);
        z-index: 10;

        svg {
          width: 16px;
          height: 16px;
        }

        &:hover {
          background: var(--color-surface-hover);
          color: var(--color-text);
          border-color: var(--color-text-muted);
          transform: scale(1.05);
        }

        &:active {
          transform: scale(0.95);
        }

        &.copied {
          color: var(--color-success);
          border-color: var(--color-success);
          background: var(--color-bg);
        }

        &.keep-visible {
          opacity: 1;
        }
      }

      &:hover .copy-button {
        opacity: 1;
      }
    }

  a {
    color: var(--color-info);
    text-decoration: none;
    transition: color var(--transition-fast);

    &:hover {
      color: var(--color-info-dark);
      text-decoration: underline;
    }
  }

  blockquote {
    border-left: 4px solid var(--color-border);
    padding-left: var(--space-lg);
    margin: var(--space-md) 0;
    color: var(--color-text-muted);
    font-style: italic;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    margin: var(--space-md) 0;
  }

  /* A table that declares column weights (a <colgroup> emitted from a
     "cols:" directive) opts into fixed layout, so the declared widths hold
     instead of competing with each column's content. Long unbreakable tokens
     then wrap inside their cell rather than pushing the column wider. */
  table:has(> colgroup) {
    table-layout: fixed;
  }

  table:has(> colgroup) th,
  table:has(> colgroup) td {
    overflow-wrap: break-word;
  }

  th,
  td {
    padding: var(--space-sm) var(--space-md);
    border: 1px solid var(--color-border);
    text-align: left;
  }

  th {
    background: var(--color-surface);
    font-weight: 600;
  }

  img {
    max-width: 100%;
    height: auto;
    border-radius: var(--radius-md);
    margin: var(--space-md) 0;
  }

  /* Code-group: tabbed code blocks for polyglot content */
  .code-group {
    margin-bottom: var(--space-md);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .code-group-tabs {
    display: flex;
    background: var(--color-surface);
    border-bottom: 1px solid var(--color-border);
    overflow-x: auto;
  }

  .code-group-tab {
    display: inline-flex;
    align-items: center;
    padding: var(--space-sm) var(--space-md);
    font-family: var(--font-sans);
    font-size: 0.85rem;
    font-weight: 500;
    color: var(--color-text-muted);
    background: transparent;
    border: none;
    border-bottom: 2px solid transparent;
    cursor: pointer;
    white-space: nowrap;
    transition: all var(--transition-fast);

    &:hover {
      color: var(--color-text);
    }

    &.active {
      color: var(--color-primary, var(--color-info));
      border-bottom-color: var(--color-primary, var(--color-info));
    }
  }

  .code-group-panels {
    position: relative;
  }

  .code-group-panel {
    display: none;

    &.active {
      display: block;
    }

    pre {
      margin: 0;
      border: none;
      border-radius: 0;
    }
  }

  .mermaid-diagram {
    margin: var(--space-lg) 0;
    padding: var(--space-lg);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    background: var(--color-bg);
    overflow-x: auto;
    text-align: center;
    min-height: 100px;

    /* Hide raw mermaid text until rendered */
    &:not([data-rendered]) {
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--color-text-muted);
      font-size: 0;

      &::after {
        font-size: 0.875rem;
        content: 'Loading diagram...';
      }
    }

    svg {
      max-width: 100%;
      height: auto;
    }
  }

  .mermaid-error {
    color: var(--color-error);
    font-size: 0.875rem;
    font-family: var(--font-mono);
    text-align: left;
    white-space: pre-wrap;
    margin: 0;
  }
`;
