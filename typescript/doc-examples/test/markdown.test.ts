import { describe, expect, test } from 'bun:test';
import { blocks, isChecked } from '../src/markdown';

describe('blocks', () => {
  test('reads fenced blocks with their info string, indentation and line', () => {
    const markdown = [
      'intro',
      '```ts nocheck',
      'a();',
      '```',
      '- item',
      '  ````tsx',
      "  const a = '```';",
      '  ````',
      '~~~',
      'plain',
    ].join('\n');
    expect(blocks(markdown)).toEqual([
      { lang: 'ts', attributes: ['nocheck'], line: 2, code: 'a();' },
      { lang: 'tsx', attributes: [], line: 6, code: "const a = '```';" },
      { lang: '', attributes: [], line: 9, code: 'plain' },
    ]);
  });

  test('the nocheck word opts a block out', () => {
    const [marked, plain] = blocks('```ts nocheck\na\n```\n```ts\nb\n```');
    expect(marked && isChecked(marked)).toBe(false);
    expect(plain && isChecked(plain)).toBe(true);
  });
});
