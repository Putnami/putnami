import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { check, type Example, LANGUAGES } from '../src/check';
import { blocks, isChecked } from '../src/markdown';

const project = join(import.meta.dir, '..');

describe('check', () => {
  test(
    'reports drift and tolerates fragments',
    () => {
      const markdown = readFileSync(join(import.meta.dir, 'fixtures/drift.md'), 'utf8');
      const examples: Example[] = blocks(markdown)
        .filter((block) => LANGUAGES.has(block.lang) && isChecked(block))
        .map((block) => ({ doc: 'drift.md', block }));
      const findings = check(examples, project).map((finding) => `${finding.line}: ${finding.message}`);
      expect(findings).toHaveLength(5);
      expect(findings[0]).toStartWith("29: Module '\"@putnami/application\"' has no exported member 'noSuchExport'");
      expect(findings[1]).toStartWith('35: ');
      expect(findings[2]).toStartWith('45: the block does not parse');
      expect(findings[3]).toStartWith('51: Expected 1-2 arguments');
      expect(findings[4]).toStartWith("58: Argument of type 'string'");
    },
    { timeout: 120_000 },
  );
});
