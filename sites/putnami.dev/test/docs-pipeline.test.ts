import { describe, expect, it } from 'bun:test';
import { extractToc } from '../src/lib/docs-loader';
import { getDocsPaths } from '../src/lib/docs/navigation.server';
import { extractHeadings, renderMarkdown } from '../src/lib/markdown';

// The SSG pre-render bakes loader output (rendered markdown + TOC) into the
// static HTML at build time, so the runtime integration suite never runs this
// pipeline. Cover it directly here — it is the source of every static doc page.

describe('renderMarkdown', () => {
  it('renders headings, paragraphs, and inline formatting to HTML', async () => {
    const html = await renderMarkdown('# Title\n\nSome **bold** text.');
    expect(html).toContain('<h1');
    expect(html).toContain('Title');
    expect(html).toContain('<strong>bold</strong>');
  });

  it('assigns slug ids to headings (used by the TOC anchors)', async () => {
    const html = await renderMarkdown('## Getting Started');
    expect(html).toMatch(/id="getting-started"/);
  });

  it('renders fenced code blocks', async () => {
    const html = await renderMarkdown('```ts\nconst x = 1;\n```');
    expect(html).toContain('<pre');
    expect(html).toContain('const x = 1;');
  });

  it('turns a cols directive into a colgroup on the following table', async () => {
    const html = await renderMarkdown('<!-- cols: 1 3 -->\n\n| A | B |\n| --- | --- |\n| a | b |');
    expect(html).toContain('<table><colgroup>');
    expect(html).toContain('width:25.0%');
    expect(html).toContain('width:75.0%');
    // The directive itself never reaches the output.
    expect(html).not.toContain('cols:');
  });

  it('drops a cols directive that does not match the table column count', async () => {
    const html = await renderMarkdown('<!-- cols: 1 2 3 -->\n\n| A | B |\n| --- | --- |\n| a | b |');
    expect(html).not.toContain('<colgroup>');
  });

  it('consumes a cols directive on the first table only', async () => {
    const html = await renderMarkdown(
      '<!-- cols: 1 3 -->\n\n| A | B |\n| --- | --- |\n| a | b |\n\n| C | D |\n| --- | --- |\n| c | d |',
    );
    expect(html.split('<colgroup>').length - 1).toBe(1);
  });

  it('passes ordinary HTML comments through untouched', async () => {
    const html = await renderMarkdown('<!-- keep me -->\n\n| A | B |\n| --- | --- |\n| a | b |');
    expect(html).toContain('<!-- keep me -->');
    expect(html).not.toContain('<colgroup>');
  });
});

describe('extractHeadings / extractToc', () => {
  it('extracts a table of contents with levels and ids', () => {
    const toc = extractHeadings('# A\n\n## B\n\n### C');
    expect(toc.map((t) => t.text)).toEqual(['A', 'B', 'C']);
    expect(toc.map((t) => t.level)).toEqual([1, 2, 3]);
    expect(toc.every((t) => typeof t.id === 'string' && t.id.length > 0)).toBe(true);
  });

  it('extractToc returns the same heading structure', async () => {
    const toc = await extractToc('# One\n\n## Two');
    expect(toc.map((t) => t.text)).toEqual(['One', 'Two']);
  });
});

describe('getDocsPaths', () => {
  it('enumerates the docs URL splats from the generated docs tree', async () => {
    const paths = await getDocsPaths();
    // The build copies docs into .gen/public/docs; in CI the integration suite
    // primes it. When present, every entry is a non-empty, prefix-free splat.
    expect(Array.isArray(paths)).toBe(true);
    for (const p of paths) {
      expect(p.length).toBeGreaterThan(0);
      expect(p.startsWith('/')).toBe(false);
    }
    if (paths.length > 0) {
      // Splats are exactly the loader lookup keys — order prefixes are stripped.
      expect(paths.some((p) => /^\d\d-/.test(p))).toBe(false);
    }
  });
});
