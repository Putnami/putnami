import { escapeHtml } from '@putnami/utils';
import { useEffect, useMemo, useRef } from 'react';
import { styled } from '../emotion';
import { MarkdownContent } from './markdown-content';
import { sanitizeHtml } from './sanitize-html';

interface MarkdownRendererProps {
  html: string;
}

const MERMAID_CDN = 'https://cdn.jsdelivr.net/npm/mermaid@11.13.0/dist/mermaid.esm.min.mjs';
const MERMAID_INTEGRITY = 'sha384-hKLcD8I+SfFa1Yk5KEK3RT3Iim6KIltmcuSq13Td5mQ+frHbnVL4E7s33UGBeAzf';
const COPY_FEEDBACK_MS = 5000;

const MarkdownRendererRoot = styled.div`
  min-width: 0;
`;

const COPY_ICON_SVG = `
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect>
    <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
  </svg>`;

const CHECK_ICON_SVG = `
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <polyline points="20 6 9 17 4 12"></polyline>
  </svg>`;

const ERROR_ICON_SVG = `
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <circle cx="12" cy="12" r="10"></circle>
    <line x1="15" y1="9" x2="9" y2="15"></line>
    <line x1="9" y1="9" x2="15" y2="15"></line>
  </svg>`;

let mermaidIdCounter = 0;
let mermaidPromise: Promise<MermaidAPI> | null = null;

interface MermaidAPI {
  initialize: (config: Record<string, unknown>) => void;
  render: (id: string, definition: string) => Promise<{ svg: string }>;
}

async function verifyAndImport(url: string, integrity: string): Promise<MermaidAPI> {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`Failed to fetch mermaid: ${response.status}`);

  const body = await response.arrayBuffer();
  const [algo, expected] = integrity.split('-', 2);
  const hash = await crypto.subtle.digest(algo.toUpperCase().replace('SHA', 'SHA-'), body);
  const actual = btoa(String.fromCharCode(...new Uint8Array(hash)));

  if (actual !== expected) {
    throw new Error('Mermaid integrity check failed — CDN response does not match expected hash');
  }

  const blob = new Blob([body], { type: 'application/javascript' });
  const blobUrl = URL.createObjectURL(blob);
  try {
    const mod = await import(/* @vite-ignore */ blobUrl);
    return mod.default as MermaidAPI;
  } finally {
    URL.revokeObjectURL(blobUrl);
  }
}

function loadMermaid(): Promise<MermaidAPI> {
  if (mermaidPromise) return mermaidPromise;
  mermaidPromise = verifyAndImport(MERMAID_CDN, MERMAID_INTEGRITY).then((mermaid) => {
    // Trust boundary: mermaid's SVG is injected via innerHTML below (markRendered)
    // WITHOUT going through this module's sanitizeHtml() allowlist — that allowlist
    // is intentionally HTML-only (no SVG element/attribute surface, no <foreignObject>),
    // and broadening it to safely admit the full SVG set mermaid emits would either
    // break diagrams or reopen the exact XSS hole the sanitizer exists to close.
    // Instead we pin mermaid's own sanitizer to its strictest level so diagram XSS
    // posture is explicit and does not silently depend on the CDN build's default:
    // 'strict' strips raw HTML in labels (no <foreignObject> HTML injection) and is
    // the only configuration under which injecting the raw SVG is acceptable.
    mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'default', fontFamily: 'inherit' });
    return mermaid;
  });
  return mermaidPromise;
}

function markRendered(diagram: HTMLElement, html: string): void {
  // The error-path strings below are built from escapeHtml()'d messages, and the
  // success-path mermaid SVG is trusted only because loadMermaid() pins mermaid to
  // securityLevel: 'strict' (see loadMermaid). This is the one sink in this module
  // that bypasses sanitizeHtml() — see the trust-boundary note in loadMermaid().
  diagram.innerHTML = html;
  diagram.dataset['rendered'] = 'true';
}

async function renderSingleDiagram(mermaid: MermaidAPI, diagram: HTMLElement): Promise<void> {
  if (diagram.dataset['rendered']) return;

  const definition = diagram.textContent?.trim() || '';
  if (!definition) {
    markRendered(diagram, '<pre class="mermaid-error">Empty diagram definition</pre>');
    return;
  }

  const id = `mermaid-${mermaidIdCounter++}`;
  try {
    const { svg } = await mermaid.render(id, definition);
    markRendered(diagram, svg);
  } catch (err) {
    const msg = escapeHtml(err instanceof Error ? err.message : 'Unknown rendering error');
    markRendered(diagram, `<pre class="mermaid-error">Diagram error: ${msg}</pre>`);
  }
}

async function renderMermaidDiagrams(container: HTMLElement): Promise<void> {
  const diagrams = Array.from(container.querySelectorAll<HTMLElement>('.mermaid-diagram'));
  if (diagrams.length === 0) return;

  try {
    const mermaid = await loadMermaid();
    await Promise.all(diagrams.map((diagram) => renderSingleDiagram(mermaid, diagram)));
  } catch (err) {
    const msg = escapeHtml(err instanceof Error ? err.message : 'Failed to load mermaid');
    for (const diagram of diagrams) {
      if (!diagram.dataset['rendered']) {
        markRendered(diagram, `<pre class="mermaid-error">Load error: ${msg}</pre>`);
      }
    }
  }
}

function syncCodeGroupTabs(container: HTMLElement, label: string): void {
  const escaped = CSS.escape(label);
  for (const group of Array.from(container.querySelectorAll<HTMLElement>('.code-group'))) {
    const matchingTab = group.querySelector<HTMLElement>(`.code-group-tab[data-label="${escaped}"]`);
    if (!matchingTab) continue;

    const targetIndex = matchingTab.dataset['index'];

    for (const t of Array.from(group.querySelectorAll<HTMLElement>('.code-group-tab'))) {
      t.classList.toggle('active', t.dataset['index'] === targetIndex);
    }
    for (const p of Array.from(group.querySelectorAll<HTMLElement>('.code-group-panel'))) {
      p.classList.toggle('active', p.dataset['index'] === targetIndex);
    }
  }
}

function addCopyButton(wrapper: HTMLElement): void {
  const button = document.createElement('button');
  button.className = 'copy-button';
  button.innerHTML = COPY_ICON_SVG;
  button.type = 'button';
  button.setAttribute('aria-label', 'Copy code to clipboard');
  wrapper.appendChild(button);
}

/**
 * Attach the interactive behaviours to a rendered-markdown container: copy
 * buttons on code blocks, persisted code-group tab switching, and mermaid
 * diagram rendering. It operates on the live DOM, so server-rendered markdown
 * can be enhanced from an island without re-shipping the HTML as props.
 *
 * Returns a cleanup that removes the listeners it added.
 */
export function enhanceMarkdown(container: HTMLElement): () => void {
  const copyToClipboard = async (text: string, button: HTMLButtonElement) => {
    try {
      await navigator.clipboard.writeText(text);
      button.innerHTML = CHECK_ICON_SVG;
      button.classList.add('copied', 'keep-visible');
      setTimeout(() => {
        button.innerHTML = COPY_ICON_SVG;
        button.classList.remove('copied', 'keep-visible');
      }, COPY_FEEDBACK_MS);
    } catch {
      button.innerHTML = ERROR_ICON_SVG;
      button.classList.add('keep-visible');
      setTimeout(() => {
        button.innerHTML = COPY_ICON_SVG;
        button.classList.remove('keep-visible');
      }, COPY_FEEDBACK_MS);
    }
  };

  const handleCopyClick = (target: HTMLElement) => {
    const wrapper = target.closest('.code-wrapper');
    const code = wrapper?.querySelector('code');
    if (code) {
      copyToClipboard(code.textContent || '', target as HTMLButtonElement);
    }
  };

  const handleCodeGroupTab = (tab: HTMLElement) => {
    const label = tab.dataset['label'];
    if (!label) return;
    try {
      localStorage.setItem('putnami-code-lang', label);
    } catch {
      // localStorage unavailable
    }
    syncCodeGroupTabs(container, label);
  };

  const handleClick = (event: MouseEvent) => {
    const target = event.target as HTMLElement;
    if (target.closest('.copy-button')) {
      handleCopyClick(target.closest('.copy-button') as HTMLElement);
      return;
    }
    const codeGroupTab = target.closest('.code-group-tab');
    if (codeGroupTab) {
      handleCodeGroupTab(codeGroupTab as HTMLElement);
    }
    // Internal links navigate natively (full-page) — no router dependency.
  };
  container.addEventListener('click', handleClick);

  // Wrap code blocks and add copy buttons (idempotent — skips wrapped blocks).
  for (const pre of Array.from(container.querySelectorAll('pre'))) {
    if (pre.closest('.code-wrapper')) continue;
    const wrapper = document.createElement('div');
    wrapper.className = 'code-wrapper';
    pre.parentNode?.insertBefore(wrapper, pre);
    wrapper.appendChild(pre);
    addCopyButton(wrapper);
  }

  // Restore the saved code-group language preference.
  let savedLabel: string | null = null;
  try {
    savedLabel = localStorage.getItem('putnami-code-lang');
  } catch {
    // localStorage unavailable
  }
  if (savedLabel) syncCodeGroupTabs(container, savedLabel);

  // Render mermaid diagrams (lazy-loads mermaid only when diagrams are present).
  void renderMermaidDiagrams(container);

  return () => container.removeEventListener('click', handleClick);
}

/**
 * Renders markdown HTML and, after mount, enhances it via {@link enhanceMarkdown}
 * (copy buttons, code-group tabs, mermaid). Internal links navigate natively (a
 * normal anchor), so the component carries no router dependency and is safe to
 * hydrate as an island (an isolated React root with no Router context). The root
 * carries `data-markdown-root` so a separate enhancer island can target it.
 *
 * The `html` prop is run through a strict allowlist sanitizer ({@link sanitizeHtml})
 * before injection, so script tags, event-handler attributes, and dangerous URL
 * schemes are removed regardless of where the HTML originated.
 */
export function MarkdownRenderer({ html }: MarkdownRendererProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const safeHtml = useMemo(() => sanitizeHtml(html), [html]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    return enhanceMarkdown(container);
  }, []);

  return (
    <MarkdownRendererRoot ref={containerRef} data-markdown-root='true'>
      {/* biome-ignore lint/security/noDangerouslySetInnerHtml: HTML is sanitized by sanitizeHtml() above (strict allowlist) before injection */}
      <MarkdownContent dangerouslySetInnerHTML={{ __html: safeHtml }} />
    </MarkdownRendererRoot>
  );
}
