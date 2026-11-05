import { describe, expect } from 'bun:test';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';

// The committed fold table the Go mapping reads is what the runtime's NFKD
// gives, and the runtime's NFKD maps the golden names to the golden IDs.
// PUTNAMI_UPDATE_GOLDEN=1 rewrites the table. (D-057)
const IDENTITY = {
  feature: 'features/feature-contracts',
  requirement: 'requirement-identity-mapping',
  check: 'the-typescript-nfkd-gives-the-golden-ids-and-the-fold-table',
};

const FEATURES = join(import.meta.dir, '../../../../../protocols/features');
const TABLE = join(FEATURES, 'unicode/ascii-fold.json');
const GOLDEN = join(FEATURES, 'fixtures/equivalence/requirement-ids.golden.json');
const NAME_MAX_LENGTH = 128;

const update = process.env['PUTNAMI_UPDATE_GOLDEN'] === '1';

interface FoldTable {
  unicode: string;
  drop: [string, string][];
  fold: Record<string, string>;
}

interface Golden {
  names: { name: string; id: string | null }[];
}

function hex(cp: number): string {
  return cp.toString(16).toUpperCase().padStart(4, '0');
}

/** What NFKD less its combining marks gives for one code point. */
function foldCodePoint(cp: number): string {
  return String.fromCodePoint(cp).normalize('NFKD').replace(/\p{M}/gu, '');
}

function deriveTable(): FoldTable {
  const drop: [string, string][] = [];
  const fold: Record<string, string> = {};
  let start = -1;
  let last = -1;
  for (let cp = 0x80; cp <= 0x10_ff_ff; cp++) {
    if (cp >= 0xd8_00 && cp <= 0xdf_ff) continue;
    const folded = foldCodePoint(cp);
    if (folded === '') {
      if (cp !== last + 1) {
        if (start >= 0) drop.push([hex(start), hex(last)]);
        start = cp;
      }
      last = cp;
    } else if (/[A-Za-z0-9]/.test(folded)) {
      fold[hex(cp)] = folded;
    }
  }
  if (start >= 0) drop.push([hex(start), hex(last)]);
  return { unicode: process.versions['unicode'] ?? '', drop, fold };
}

/** One entry per line, so a Unicode update reads as a short diff. */
function serializeTable(table: FoldTable): string {
  const drop = table.drop.map((range) => `    ${JSON.stringify(range)}`).join(',\n');
  const fold = Object.entries(table.fold)
    .map(([cp, folded]) => `    ${JSON.stringify(cp)}: ${JSON.stringify(folded)}`)
    .join(',\n');
  return `{\n  "unicode": ${JSON.stringify(table.unicode)},\n  "drop": [\n${drop}\n  ],\n  "fold": {\n${fold}\n  }\n}\n`;
}

/** The D-057 mapping, with the runtime's NFKD in place of the Go table. */
function requirementId(name: string): string | null {
  const folded = name.normalize('NFKD').replace(/\p{M}/gu, '');
  let id = '';
  let pending = false;
  for (const char of folded) {
    if (/^[A-Za-z0-9]$/.test(char)) {
      if (pending && id !== '') id += '-';
      pending = false;
      id += char.toLowerCase();
    } else {
      pending = true;
    }
  }
  return id === '' || id.length > NAME_MAX_LENGTH ? null : id;
}

describe('requirement IDs', () => {
  specTest('the committed fold table is what the runtime NFKD gives', IDENTITY, () => {
    const derived = deriveTable();
    if (update) writeFileSync(TABLE, serializeTable(derived));
    const committed = JSON.parse(readFileSync(TABLE, 'utf8')) as FoldTable;
    if (derived.unicode === committed.unicode) {
      expect(derived).toEqual(committed);
      return;
    }
    // Unicode keeps a decomposition once assigned, so a newer runtime only
    // adds code points: every committed one must still fold the same way.
    const derivedDrop = new Set<number>();
    for (const [low, high] of derived.drop) {
      for (let cp = Number.parseInt(low, 16); cp <= Number.parseInt(high, 16); cp++) derivedDrop.add(cp);
    }
    for (const [low, high] of committed.drop) {
      for (let cp = Number.parseInt(low, 16); cp <= Number.parseInt(high, 16); cp++) {
        expect([hex(cp), derivedDrop.has(cp)]).toEqual([hex(cp), true]);
      }
    }
    for (const [cp, folded] of Object.entries(committed.fold)) {
      expect([cp, derived.fold[cp]]).toEqual([cp, folded]);
    }
  });

  specTest('the golden names map to the golden IDs', IDENTITY, () => {
    const golden = JSON.parse(readFileSync(GOLDEN, 'utf8')) as Golden;
    expect(golden.names.length).toBeGreaterThan(0);
    for (const { name, id } of golden.names) {
      expect([name, requirementId(name)]).toEqual([name, id]);
    }
  });
});
