/**
 * Client-side helper for the "Copy as Markdown" agent affordance.
 *
 * Doc pages have their raw source served by the `/doc-markdown` endpoint, so the
 * highest-fidelity payload — real Markdown with intact code fences, headings,
 * and links — is fetched lazily only when a reader copies. If that's
 * unavailable (non-doc pages, or no runtime), we fall back to the rendered
 * article text so the button always does something useful.
 */

/** Fetch the page's raw source markdown from the doc-markdown endpoint, if any. */
async function fetchSourceMarkdown(): Promise<string | undefined> {
  const path = window.location.pathname.replace(/\/+$/, '');
  if (!path.startsWith('/docs/') || path === '/docs') return undefined;
  try {
    const res = await fetch(`/doc-markdown?path=${encodeURIComponent(path)}`, {
      headers: { accept: 'text/markdown' },
    });
    if (!res.ok) return undefined;
    const markdown = (await res.text()).trim();
    return markdown || undefined;
  } catch {
    return undefined;
  }
}

/** Best-effort Markdown reconstruction from the rendered DOM. */
function renderedFallback(title: string): string {
  const root = document.querySelector('[data-markdown-root]');
  const body = root instanceof HTMLElement ? root.innerText.trim() : '';
  return body ? `# ${title}\n\n${body}` : `# ${title}`;
}

export async function copyPageAsMarkdown(): Promise<boolean> {
  if (typeof document === 'undefined') return false;
  const url = window.location.href;
  const title = document.title.split(' — ')[0] ?? document.title;

  const source = await fetchSourceMarkdown();
  const markdown = source ?? renderedFallback(title);
  const payload = `${markdown}\n\nSource: ${url}`;

  try {
    await navigator.clipboard.writeText(payload);
    return true;
  } catch {
    return false;
  }
}
