'use client';

import { useTrack } from '@putnami/analytics';
import { styled } from '@putnami/ui';
import { type ChangeEvent, type KeyboardEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { copyPageAsMarkdown } from '../lib/page-markdown';
import type { SearchResult } from '../lib/search/types';
import { pageCountLabel, TOOL_ORDER, TOOLS } from '../lib/tools';
import { ToolGlyph } from './tool-glyph';

// Debounce delay in ms
const DEBOUNCE_MS = 80;

const COLOR_MODE_STORAGE_KEY = 'putnami-color-mode';

interface SearchModalProps {
  isOpen: boolean;
  onClose: () => void;
}

// ── Command palette entries ────────────────────────────────────────────────
// The palette is two things at once: a scoped doc search AND a command runner.
// Actions + tool jumps render above page results; everything is one keyboard
// list. Commands are "islands" of behaviour the static site can't express
// inline (theme toggle, copy-for-LLM), surfaced where they're discoverable.

type ActionId = 'theme' | 'copymd';

interface ActionRow {
  kind: 'action';
  id: ActionId;
  label: string;
  hint: string;
}

interface ToolRowData {
  kind: 'tool';
  id: string;
  label: string;
  hint: string;
  href: string;
}

interface ResultRow {
  kind: 'result';
  result: SearchResult;
}

type Row = ActionRow | ToolRowData | ResultRow;

const ACTIONS: ActionRow[] = [
  { kind: 'action', id: 'theme', label: 'Toggle theme', hint: 'light / dark' },
  { kind: 'action', id: 'copymd', label: 'Copy page as Markdown', hint: 'for LLMs & agents' },
];

const TOOL_ROWS: ToolRowData[] = TOOL_ORDER.map((id) => ({
  kind: 'tool',
  id,
  label: `${TOOLS[id].short} docs`,
  hint: pageCountLabel(TOOLS[id].pages),
  href: TOOLS[id].href,
}));

/** Flip the color mode the same way the ThemeProvider does: drive the
 * `data-color-mode` attribute (which the global CSS variables key off) and
 * persist to localStorage so a full-page navigation keeps the choice. */
function toggleColorMode(): void {
  const current = document.documentElement.dataset['colorMode'] === 'dark' ? 'dark' : 'light';
  const next = current === 'dark' ? 'light' : 'dark';
  document.documentElement.dataset['colorMode'] = next;
  document.documentElement.style.colorScheme = next;
  try {
    localStorage.setItem(COLOR_MODE_STORAGE_KEY, next);
  } catch {
    // storage may be unavailable (private mode); the attribute change still applies
  }
}

/**
 * Highlight matching query tokens in text using <mark> tags.
 * Returns an array of React elements.
 */
function highlightMatches(text: string, queryTokens: string[]): React.ReactNode[] {
  if (!queryTokens.length || !text) return [text];

  // Build a regex matching any token
  const escaped = queryTokens.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
  const regex = new RegExp(`(${escaped.join('|')})`, 'gi');
  const parts = text.split(regex);

  // Use character offset as key instead of array index
  const result: React.ReactNode[] = [];
  let offset = 0;
  for (const part of parts) {
    if (regex.test(part)) {
      result.push(<Mark key={`mark-${offset}`}>{part}</Mark>);
    } else {
      result.push(part);
    }
    offset += part.length;
  }
  return result;
}

export function SearchModal({ isOpen, onClose }: SearchModalProps) {
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SearchResult[]>([]);
  const [activeIndex, setActiveIndex] = useState(0);
  const [queryTokens, setQueryTokens] = useState<string[]>([]);
  const [copied, setCopied] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Reads `window.__putnamiAnalytics` at call time, so it is a no-op wherever
  // the tracker is not installed — during SSR, and in the docs prerender.
  const track = useTrack();

  // Focus input when modal opens
  useEffect(() => {
    if (!isOpen) return;
    setQuery('');
    setResults([]);
    setActiveIndex(0);
    setQueryTokens([]);
    setCopied(false);
    // Small delay to ensure portal is rendered
    const t = setTimeout(() => inputRef.current?.focus(), 50);
    return () => clearTimeout(t);
  }, [isOpen]);

  // Lock body scroll when open
  useEffect(() => {
    if (!isOpen) return;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = '';
    };
  }, [isOpen]);

  const performSearch = useCallback(async (q: string) => {
    if (!q.trim()) {
      setResults([]);
      setQueryTokens([]);
      return;
    }

    // Dynamic import to keep bundle small - only load search code when needed
    const { search, tokenize } = await import('../lib/search');
    const tokens = tokenize(q.trim());
    setQueryTokens(tokens);
    const searchResults = await search(q, 10);
    setResults(searchResults);
  }, []);

  const handleInputChange = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => {
      const value = e.target.value;
      setQuery(value);
      setActiveIndex(0);
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => performSearch(value), DEBOUNCE_MS);
    },
    [performSearch],
  );

  // Build the combined, keyboard-navigable row list (the "selectable" entries).
  const rows = useMemo<Row[]>(() => {
    const q = query.trim().toLowerCase();
    const actions = ACTIONS.filter((a) => !q || a.label.toLowerCase().includes(q));
    const tools = TOOL_ROWS.filter((t) => !q || t.label.toLowerCase().includes(q));
    const pages: ResultRow[] = results.map((result) => ({ kind: 'result', result }));
    return [...actions, ...tools, ...pages];
  }, [query, results]);

  const navigate = useCallback(
    (url: string) => {
      onClose();
      // Router-free: the modal hydrates in an isolated island root with no
      // Router context, so navigate via a full-page load (correct on a
      // zero-base-JS static site).
      window.location.assign(url);
    },
    [onClose],
  );

  const runRow = useCallback(
    (row: Row | undefined) => {
      if (!row) return;
      if (row.kind === 'tool') {
        navigate(row.href);
        return;
      }
      if (row.kind === 'result') {
        // The query itself is never sent: it is text a visitor typed. How many
        // results it produced is what says whether the search is working.
        track('docs_search_open', { hits: results.length });
        navigate(row.result.url);
        return;
      }
      // Action rows keep the palette open after running.
      if (row.id === 'theme') {
        toggleColorMode();
        return;
      }
      copyPageAsMarkdown()
        .then((ok) => {
          if (ok) {
            track('doc_copy_markdown', { surface: 'palette' });
            setCopied(true);
            setTimeout(() => setCopied(false), 1600);
          }
        })
        .catch(() => {});
    },
    [navigate, results.length, track],
  );

  const scrollToItem = useCallback((index: number) => {
    const list = listRef.current;
    if (!list) return;
    const item = list.querySelector<HTMLElement>(`[data-row-index="${index}"]`);
    if (item) item.scrollIntoView({ block: 'nearest' });
  }, []);

  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          setActiveIndex((prev) => {
            const next = Math.min(prev + 1, rows.length - 1);
            scrollToItem(next);
            return next;
          });
          break;
        case 'ArrowUp':
          e.preventDefault();
          setActiveIndex((prev) => {
            const next = Math.max(prev - 1, 0);
            scrollToItem(next);
            return next;
          });
          break;
        case 'Enter':
          e.preventDefault();
          runRow(rows[activeIndex]);
          break;
        case 'Escape':
          e.preventDefault();
          onClose();
          break;
      }
    },
    [rows, activeIndex, runRow, onClose, scrollToItem],
  );

  if (!isOpen || typeof document === 'undefined') return null;

  const hasQuery = query.trim().length > 0;
  const groupLabel = (kind: Row['kind']): string =>
    kind === 'action' ? 'Actions' : kind === 'tool' ? 'Go to surface' : hasQuery ? 'Pages' : 'Suggested pages';

  return createPortal(
    <Overlay onClick={onClose}>
      <Dialog onClick={(e) => e.stopPropagation()} onKeyDown={handleKeyDown}>
        <SearchHeader>
          <SearchIcon />
          <SearchInput
            ref={inputRef}
            type='text'
            placeholder='Search docs or run a command…'
            value={query}
            onChange={handleInputChange}
            aria-label='Search documentation or run a command'
            aria-activedescendant={rows[activeIndex] ? `palette-row-${activeIndex}` : undefined}
            role='combobox'
            aria-expanded={rows.length > 0}
            aria-controls='palette-results'
            aria-autocomplete='list'
          />
          <ScopeChip>docs</ScopeChip>
        </SearchHeader>

        {rows.length > 0 && (
          <ResultsList ref={listRef} id='palette-results' role='listbox'>
            {rows.map((row, i) => (
              <PaletteRow
                key={rowKey(row)}
                row={row}
                index={i}
                active={i === activeIndex}
                copied={copied}
                queryTokens={queryTokens}
                header={i === 0 || rows[i - 1].kind !== row.kind ? groupLabel(row.kind) : null}
                onRun={runRow}
                onHover={setActiveIndex}
              />
            ))}
          </ResultsList>
        )}

        {hasQuery && rows.length === 0 && (
          <EmptyState>
            <EmptyIcon />
            <span>No matches for &ldquo;{query}&rdquo;</span>
          </EmptyState>
        )}

        <SearchFooter>
          <FooterHint>
            <Kbd>↑</Kbd>
            <Kbd>↓</Kbd>
            <span>navigate</span>
          </FooterHint>
          <FooterHint>
            <Kbd>↵</Kbd>
            <span>select</span>
          </FooterHint>
          <FooterHint>
            <Kbd>esc</Kbd>
            <span>close</span>
          </FooterHint>
        </SearchFooter>
      </Dialog>
    </Overlay>,
    document.body,
  );
}

function PaletteRow({
  row,
  index,
  active,
  copied,
  queryTokens,
  header,
  onRun,
  onHover,
}: {
  row: Row;
  index: number;
  active: boolean;
  copied: boolean;
  queryTokens: string[];
  header: string | null;
  onRun: (row: Row) => void;
  onHover: (index: number) => void;
}) {
  return (
    <div>
      {header && <GroupHeader>{header}</GroupHeader>}
      <ResultItem
        id={`palette-row-${index}`}
        data-row-index={index}
        role='option'
        aria-selected={active}
        $active={active}
        onClick={() => onRun(row)}
        onMouseEnter={() => onHover(index)}
      >
        <RowIcon row={row} copied={copied} />
        <RowBody>
          <ResultTitle>{rowTitle(row, queryTokens, copied)}</ResultTitle>
          <ResultMeta>{rowMeta(row, queryTokens)}</ResultMeta>
        </RowBody>
        {active && <EnterHint>↵</EnterHint>}
      </ResultItem>
    </div>
  );
}

function rowKey(row: Row): string {
  if (row.kind === 'action') return `action-${row.id}`;
  if (row.kind === 'tool') return `tool-${row.id}`;
  return `result-${row.result.url}`;
}

function rowTitle(row: Row, tokens: string[], copied: boolean): React.ReactNode {
  if (row.kind === 'action') {
    return row.id === 'copymd' && copied ? 'Copied for LLM' : row.label;
  }
  if (row.kind === 'tool') return row.label;
  return highlightMatches(row.result.title, tokens);
}

function rowMeta(row: Row, tokens: string[]): React.ReactNode {
  if (row.kind === 'action') return row.hint;
  if (row.kind === 'tool') return row.hint;
  const { breadcrumbs, snippet } = row.result;
  if (breadcrumbs.length > 0) return breadcrumbs.join(' / ');
  if (snippet) return highlightMatches(snippet, tokens);
  return null;
}

function RowIcon({ row, copied }: { row: Row; copied: boolean }) {
  if (row.kind === 'tool') return <ToolGlyph tool={row.id} size={28} />;
  if (row.kind === 'action') {
    return <IconBox>{row.id === 'theme' ? <ThemeGlyphIcon /> : copied ? <CheckIcon /> : <ClipboardIcon />}</IconBox>;
  }
  return (
    <IconBox>
      <FileIcon />
    </IconBox>
  );
}

// ---------------------------------------------------------------------------
// Hook: Cmd+K / Ctrl+K keyboard shortcut to open search
// ---------------------------------------------------------------------------

export function useDocSearch() {
  const [isOpen, setIsOpen] = useState(false);

  useEffect(() => {
    const handler = (e: globalThis.KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        setIsOpen((prev) => !prev);
      }
    };

    document.addEventListener('keydown', handler);
    return () => document.removeEventListener('keydown', handler);
  }, []);

  const open = useCallback(() => setIsOpen(true), []);
  const close = useCallback(() => setIsOpen(false), []);

  return { isOpen, open, close };
}

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function SearchIcon() {
  return (
    <svg
      width='20'
      height='20'
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

function ThemeGlyphIcon() {
  return (
    <svg
      width='16'
      height='16'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <circle cx='12' cy='12' r='4' />
      <path d='M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41' />
    </svg>
  );
}

function ClipboardIcon() {
  return (
    <svg
      width='15'
      height='15'
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
      width='15'
      height='15'
      viewBox='0 0 24 24'
      fill='none'
      stroke='var(--color-success)'
      strokeWidth='2.5'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M20 6 9 17l-5-5' />
    </svg>
  );
}

function FileIcon() {
  return (
    <svg
      width='15'
      height='15'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z' />
      <path d='M14 2v6h6' />
    </svg>
  );
}

function EmptyIcon() {
  return (
    <svg
      width='40'
      height='40'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='1.5'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
      style={{ opacity: 0.4 }}
    >
      <circle cx='11' cy='11' r='8' />
      <line x1='21' y1='21' x2='16.65' y2='16.65' />
      <line x1='8' y1='11' x2='14' y2='11' />
    </svg>
  );
}

// ---------------------------------------------------------------------------
// Styled components
// ---------------------------------------------------------------------------

const Overlay = styled.div`
  position: fixed;
  inset: 0;
  z-index: 1100;
  background: rgba(8, 8, 10, 0.55);
  backdrop-filter: blur(3px);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding-top: min(14vh, 110px);
`;

const Dialog = styled.div`
  width: 90%;
  max-width: 620px;
  max-height: 70vh;
  background: var(--color-bg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-xl, 0 25px 50px -12px rgba(0, 0, 0, 0.25));
  display: flex;
  flex-direction: column;
  overflow: hidden;
`;

const SearchHeader = styled.div`
  display: flex;
  align-items: center;
  gap: var(--space-sm);
  padding: 14px 16px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-text-dim);
`;

const SearchInput = styled.input`
  flex: 1;
  border: none;
  outline: none;
  background: transparent;
  color: var(--color-text);
  font-size: 1rem;
  font-family: var(--font-sans);
  padding: var(--space-xs) 0;

  &::placeholder {
    color: var(--color-text-muted);
  }
`;

const ScopeChip = styled.span`
  display: inline-flex;
  align-items: center;
  height: 22px;
  padding: 0 8px;
  font-size: 0.7rem;
  font-weight: 500;
  font-family: var(--font-mono);
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  color: var(--color-text-muted);
  flex-shrink: 0;
`;

const ResultsList = styled.div`
  flex: 1;
  overflow-y: auto;
  padding: var(--space-xs) 6px;
`;

const GroupHeader = styled.div`
  padding: 10px 12px 4px;
  font-size: 0.7rem;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--color-text-dim);
`;

const ResultItem = styled.div<{ $active: boolean }>`
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 9px 12px;
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: background var(--transition-fast);
  background: ${({ $active }) => ($active ? 'var(--color-surface-hover)' : 'transparent')};

  &:hover {
    background: var(--color-surface-hover);
  }
`;

const IconBox = styled.span`
  width: 28px;
  height: 28px;
  flex: 0 0 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  color: var(--color-text-muted);
`;

const RowBody = styled.span`
  flex: 1;
  min-width: 0;
`;

const ResultTitle = styled.div`
  font-weight: 500;
  font-size: 0.85rem;
  color: var(--color-text);
  font-family: var(--font-sans);
`;

const ResultMeta = styled.div`
  font-size: 0.75rem;
  color: var(--color-text-muted);
  margin-top: 2px;
  font-family: var(--font-sans);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
`;

const EnterHint = styled.span`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  height: 18px;
  min-width: 18px;
  padding: 0 5px;
  font-size: 0.65rem;
  font-family: var(--font-mono);
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  color: var(--color-text-muted);
  flex-shrink: 0;
`;

const Mark = styled.mark`
  background: color-mix(in srgb, var(--color-primary) 22%, transparent);
  color: inherit;
  border-radius: 2px;
  padding: 0 1px;
`;

const EmptyState = styled.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-sm);
  padding: var(--space-2xl);
  color: var(--color-text-muted);
  font-size: 0.9rem;
  font-family: var(--font-sans);
`;

const SearchFooter = styled.div`
  display: flex;
  align-items: center;
  gap: var(--space-md);
  padding: var(--space-xs) var(--space-md);
  border-top: 1px solid var(--color-border);
  font-size: 0.75rem;
  color: var(--color-text-muted);
  font-family: var(--font-sans);
`;

const FooterHint = styled.div`
  display: flex;
  align-items: center;
  gap: 4px;
`;

const Kbd = styled.kbd`
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 20px;
  height: 20px;
  padding: 0 4px;
  font-size: 0.7rem;
  font-family: var(--font-sans);
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 4px;
  color: var(--color-text-muted);
`;
