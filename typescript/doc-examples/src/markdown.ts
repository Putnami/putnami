/**
 * The info-string word that opts a fenced block out of the check: "```ts
 * nocheck" marks a fragment that is not meant to compile, such as pseudo-code
 * or a block with elisions.
 */
export const NO_CHECK = 'nocheck';

/** One fenced code block of a markdown document. */
export interface Block {
  /** The first word of the info string, lowercased. */
  lang: string;
  /** The other words of the info string. */
  attributes: string[];
  /** The 1-based line of the opening fence. */
  line: number;
  /** The block's content, with the fence's indentation removed. */
  code: string;
}

/** Whether the block carries no opt-out marker. */
export function isChecked(block: Block): boolean {
  return !block.attributes.includes(NO_CHECK);
}

const OPENING = /^([ \t]*)(`{3,}|~{3,})(.*)$/;

/**
 * Returns the fenced code blocks of a markdown document, in order. A fence is
 * three or more backticks or tildes; the block ends at a fence of the same
 * character at least as long, or at the end of the document.
 */
export function blocks(markdown: string): Block[] {
  const lines = markdown.split('\n');
  const found: Block[] = [];
  for (let i = 0; i < lines.length; i++) {
    const match = OPENING.exec(lines[i] ?? '');
    if (!match) continue;
    const indent = match[1]?.length ?? 0;
    const fence = match[2] ?? '';
    const info = (match[3] ?? '').trim();
    if (fence.startsWith('`') && info.includes('`')) continue;
    const words = info.split(/\s+/).filter(Boolean);
    const body: string[] = [];
    let j = i + 1;
    for (; j < lines.length; j++) {
      const line = lines[j] ?? '';
      if (isClosing(line, fence)) break;
      body.push(dedent(line, indent));
    }
    found.push({
      lang: (words[0] ?? '').toLowerCase(),
      attributes: words.slice(1),
      line: i + 1,
      code: body.join('\n'),
    });
    i = j;
  }
  return found;
}

function isClosing(line: string, fence: string): boolean {
  const trimmed = line.trim();
  if (trimmed.length < fence.length || trimmed[0] !== fence[0]) return false;
  return trimmed.split('').every((char) => char === fence[0]);
}

function dedent(line: string, indent: number): string {
  let n = 0;
  while (n < indent && n < line.length && (line[n] === ' ' || line[n] === '\t')) n++;
  return line.slice(n);
}
