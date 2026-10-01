import { type Highlighter as ShikiHighlighter, createHighlighter } from 'shiki';
import { extractHeadings, type Highlighter, renderMarkdown } from './markdown';

let highlighterPromise: Promise<ShikiHighlighter> | null = null;

function getHighlighter(): Promise<ShikiHighlighter> {
  if (!highlighterPromise) {
    highlighterPromise = createHighlighter({
      themes: ['github-dark', 'github-light'],
      langs: [
        'bash',
        'typescript',
        'tsx',
        'javascript',
        'jsx',
        'json',
        'yaml',
        'markdown',
        'html',
        'css',
        'sql',
        'go',
        'python',
      ],
    });
  }
  return highlighterPromise;
}

export async function shikiHighlighter(): Promise<Highlighter> {
  const hl = await getHighlighter();
  return (code, lang) => {
    const supportedLang = hl.getLoadedLanguages().includes(lang) ? lang : 'text';
    return hl.codeToHtml(code, {
      lang: supportedLang,
      themes: { light: 'github-light', dark: 'github-dark' },
      defaultColor: false,
    });
  };
}

/**
 * Build-time markdown renderer with syntax highlighting via shiki.
 * Used by the docs prerender plugin; runtime code uses the shiki-free
 * `renderMarkdown` export from `./markdown`.
 */
export async function renderMarkdownWithHighlighting(markdown: string): Promise<string> {
  const highlight = await shikiHighlighter();
  return renderMarkdown(markdown, highlight);
}

export { extractHeadings };
