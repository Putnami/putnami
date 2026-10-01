import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// Asserts the public observability boundary at the source level: the package
// entry point must not re-export the internal module wholesale or surface any
// of its `record*` emitters. A source-text check (rather than importing the
// barrel) keeps this test independent of the cross-package module graph — every
// other test in this package imports specific submodules, never `../src/index`.
const entryPath = fileURLToPath(new URL('../src/index.ts', import.meta.url));
const entrySource = readFileSync(entryPath, 'utf8');

describe('@putnami/document public API surface', () => {
  it('does not re-export the observability module wholesale', () => {
    expect(entrySource).not.toMatch(/export\s+\*\s+from\s+['"]\.\/observability['"]/);
  });

  it('does not expose internal record* emitters from the entry point', () => {
    // Only the metric *types* are public; the emitters stay internal.
    expect(entrySource).not.toMatch(/\brecordDocumentOp\b/);
    expect(entrySource).not.toMatch(/\brecordDocumentError\b/);
    expect(entrySource).not.toMatch(/\brecordSlowDocumentOp\b/);
    expect(entrySource).not.toMatch(/\brecordBackend\w+\b/);
    expect(entrySource).not.toMatch(/\brecordTransaction\w+\b/);
  });

  it('still exports the public observability types', () => {
    expect(entrySource).toMatch(/export\s+type\s+\{[^}]*\bDocumentMetrics\b/);
    expect(entrySource).toMatch(/export\s+type\s+\{[^}]*\bDocumentOperation\b/);
  });
});
