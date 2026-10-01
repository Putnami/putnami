import { describe, expect, it } from 'bun:test';
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { specTest } from '@putnami/spectest';
import { joinPath } from '@putnami/utils';
import {
  type SupportCatalog,
  type SupportEntry,
  type SupportKind,
  parseSupportCatalog,
  renderSupportPage,
  statusLabel,
  statusOf,
} from '../src/lib/support/catalog';
import { readSupportCatalog, SUPPORT_CATALOG_FILENAME } from '../src/lib/support/catalog.server';
import { TOOL_ORDER, TOOLS, statusMeta } from '../src/lib/tools';

/**
 * The reviewed catalog at the workspace root is the ONLY authority for a public
 * support status. These tests protect the two properties that make publishing
 * it safe: the site cannot render a status the catalog does not hold, and it
 * cannot silently accept a catalog it does not fully understand.
 */

const WORKSPACE_ROOT = joinPath(import.meta.dir, '..', '..', '..');

function reviewedCatalog(): SupportCatalog {
  return parseSupportCatalog(readFileSync(joinPath(WORKSPACE_ROOT, SUPPORT_CATALOG_FILENAME), 'utf8'));
}

function catalogOf(entries: unknown[], protocolVersion: unknown = 1): string {
  return JSON.stringify({ protocolVersion, entries });
}

interface PublishedSupportRow {
  kind: SupportKind;
  id: string;
  status: string;
  claims: string;
}

function publishedSupportRows(page: string): PublishedSupportRow[] {
  const rows: PublishedSupportRow[] = [];
  let kind: SupportKind | undefined;

  for (const line of page.split('\n')) {
    if (line === '## Packages') kind = 'package';
    if (line === '## Protocols') kind = 'protocol';
    if (line === '## Features') kind = 'feature';
    if (line === '## What is not on this page') kind = undefined;
    if (!kind) continue;

    const match = line.match(/^\| `([^`]+)` \| (Stable|Preview|Experimental) \| (.+) \|$/);
    if (!match) continue;
    rows.push({ kind, id: match[1] as string, status: match[2] as string, claims: match[3] as string });
  }
  return rows;
}

function expectedClaims(entry: SupportEntry): string {
  const claims: string[] = [];
  if (entry.default === false) claims.push('not default');
  if (entry.parity === 'unsupported') claims.push('no cross-implementation parity promise');
  return claims.length > 0 ? claims.join('; ') : '—';
}

describe('shared fixture corpus conformance', () => {
  // protocols/support ships one invalid fixture per diagnostic code and asserts
  // its own parser rejects each. Asserting the SAME corpus here is what keeps
  // the two implementations from drifting: a new rejection added upstream
  // fails this suite until this reader learns it too.
  const fixtures = (kind: 'valid' | 'invalid') => joinPath(WORKSPACE_ROOT, 'protocols', 'support', 'fixtures', kind);

  it('accepts every valid fixture', () => {
    const dir = fixtures('valid');
    const files = readdirSync(dir).filter((file) => file.endsWith('.json'));
    expect(files.length).toBeGreaterThan(0);
    for (const file of files) {
      expect(() => parseSupportCatalog(readFileSync(joinPath(dir, file), 'utf8'))).not.toThrow();
    }
  });

  it('rejects every invalid fixture', () => {
    const dir = fixtures('invalid');
    const files = readdirSync(dir).filter((file) => file.endsWith('.json'));
    expect(files.length).toBeGreaterThan(0);
    const accepted: string[] = [];
    for (const file of files) {
      try {
        parseSupportCatalog(readFileSync(joinPath(dir, file), 'utf8'));
        accepted.push(file);
      } catch {
        // rejected, as the protocol requires
      }
    }
    expect(accepted).toEqual([]);
  });
});

describe('strict reading (JSON.parse is not enough)', () => {
  const entry = '{ "id": "@putnami/python", "kind": "package", "status": "experimental" }';

  it('rejects a protocolVersion written as a non-integer token', () => {
    // JSON.parse turns 1.0, 1e0, and 1 into the same JavaScript number, so this
    // can only be caught on the raw token.
    for (const token of ['1.0', '1e0', '1.00', '0.1e1']) {
      expect(() => parseSupportCatalog(`{"protocolVersion": ${token}, "entries": [${entry}]}`)).toThrow(
        'exact integer token 1',
      );
    }
  });

  it('accepts the exact integer token 1', () => {
    expect(() => parseSupportCatalog(`{"protocolVersion": 1, "entries": [${entry}]}`)).not.toThrow();
  });

  it('rejects duplicate object fields instead of silently keeping the last', () => {
    const source = `{"protocolVersion": 1, "entries": [${entry}], "entries": []}`;
    expect(JSON.parse(source).entries).toEqual([]); // what a lossy read would publish
    expect(() => parseSupportCatalog(source)).toThrow('appears more than once');
  });

  it('rejects a duplicate field inside an entry', () => {
    const source =
      '{"protocolVersion": 1, "entries": [{ "id": "a", "kind": "package", "status": "stable", "status": "experimental" }]}';
    expect(() => parseSupportCatalog(source)).toThrow('appears more than once');
  });

  it('rejects an explicit null', () => {
    expect(() =>
      parseSupportCatalog('{"protocolVersion": 1, "entries": [{ "id": "a", "kind": "package", "status": null }]}'),
    ).toThrow('explicit null');
  });

  it('rejects trailing JSON after the document', () => {
    expect(() => parseSupportCatalog(`{"protocolVersion": 1, "entries": [${entry}]} {}`)).toThrow('trailing JSON');
  });

  it('rejects an unknown field on the catalog', () => {
    expect(() => parseSupportCatalog(`{"protocolVersion": 1, "entries": [${entry}], "maturity": "coded"}`)).toThrow(
      'unknown field',
    );
  });

  it('rejects an unknown field on an entry', () => {
    // `maturity` is the dangerous one: it belongs to the evidence ladder, and a
    // support entry carrying one would assert a second, conflicting vocabulary.
    expect(() =>
      parseSupportCatalog(
        '{"protocolVersion": 1, "entries": [{ "id": "a", "kind": "package", "status": "stable", "maturity": "coded" }]}',
      ),
    ).toThrow('unknown field "maturity"');
  });

  it('accepts the optional $schema field the reviewed catalog carries', () => {
    expect(() =>
      parseSupportCatalog(
        `{"$schema": "https://putnami.dev/schemas/putnami-support.json", "protocolVersion": 1, "entries": [${entry}]}`,
      ),
    ).not.toThrow();
  });

  it('rejects an empty entry list', () => {
    expect(() => parseSupportCatalog('{"protocolVersion": 1, "entries": []}')).toThrow('at least one');
  });

  it('rejects a non-canonical subject id', () => {
    for (const id of ['@Putnami/Python', 'a//b', 'a/../b', 'a/./b', '/leading', '-leading']) {
      expect(() =>
        parseSupportCatalog(
          `{"protocolVersion": 1, "entries": [{ "id": ${JSON.stringify(id)}, "kind": "package", "status": "stable" }]}`,
        ),
      ).toThrow();
    }
  });
});

describe('support catalog parsing', () => {
  it('parses the reviewed workspace catalog', () => {
    const catalog = reviewedCatalog();
    expect(catalog.protocolVersion).toBe(1);
    expect(catalog.entries.length).toBeGreaterThan(0);
  });

  specTest(
    'rejects a protocolVersion token other than the exact integer 1',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'invalid-catalog-fails-the-build',
      check: 'unsupported-protocol-version-fails',
    },
    () => {
      expect(() => parseSupportCatalog(catalogOf([], '1'))).toThrow('exact integer token 1');
      expect(() => parseSupportCatalog(catalogOf([], 2))).toThrow('exact integer token 1');
    },
  );

  specTest(
    'rejects a status outside the closed vocabulary',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'closed-status-vocabulary',
      check: 'unknown-status-token-fails-before-rendering',
    },
    () => {
      // `beta` and `evolving` are prose, not wire values. A page that rendered
      // one would be showing a status no reviewed catalog can hold.
      for (const status of ['evolving', 'beta', 'deprecated']) {
        expect(() => parseSupportCatalog(catalogOf([{ id: 'x', kind: 'package', status }]))).toThrow(
          'closed status vocabulary',
        );
      }
    },
  );

  it('rejects a subject kind outside the closed vocabulary', () => {
    expect(() => parseSupportCatalog(catalogOf([{ id: 'x', kind: 'site', status: 'stable' }]))).toThrow(
      'closed subject-kind vocabulary',
    );
  });

  specTest(
    'rejects a duplicate (kind, id) identity',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'invalid-catalog-fails-the-build',
      check: 'duplicate-subject-identity-fails',
    },
    () => {
      expect(() =>
        parseSupportCatalog(
          catalogOf([
            { id: 'x', kind: 'package', status: 'stable' },
            { id: 'x', kind: 'package', status: 'preview' },
          ]),
        ),
      ).toThrow('duplicate package entry');
    },
  );

  it('accepts the same id under two different kinds', () => {
    const catalog = parseSupportCatalog(
      catalogOf([
        { id: 'x', kind: 'package', status: 'stable' },
        { id: 'x', kind: 'protocol', status: 'preview' },
      ]),
    );
    expect(statusOf(catalog, { kind: 'package', id: 'x' })).toBe('stable');
    expect(statusOf(catalog, { kind: 'protocol', id: 'x' })).toBe('preview');
  });

  specTest(
    'rejects an experimental entry that also claims to be default-on',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'invalid-catalog-fails-the-build',
      check: 'experimental-default-on-fails',
    },
    () => {
      expect(() =>
        parseSupportCatalog(catalogOf([{ id: 'x', kind: 'package', status: 'experimental', default: true }])),
      ).toThrow('cannot also be default-on');
    },
  );

  it('rejects a parity value other than "unsupported"', () => {
    expect(() =>
      parseSupportCatalog(catalogOf([{ id: 'x', kind: 'package', status: 'stable', parity: 'supported' }])),
    ).toThrow('accepts only "unsupported"');
  });

  it('reports no status for a subject the catalog does not classify', () => {
    // Absence means "no reviewed status", never "assume stable".
    expect(statusOf(reviewedCatalog(), { kind: 'package', id: 'not-a-real-subject' })).toBeUndefined();
  });

  specTest(
    'fails when the reviewed workspace catalog cannot be read',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'invalid-catalog-fails-the-build',
      check: 'unreadable-catalog-fails',
    },
    () => {
      const workspaceRoot = mkdtempSync(joinPath(tmpdir(), 'putnami-support-missing-'));
      try {
        expect(() => readSupportCatalog(workspaceRoot)).toThrow('cannot read');
      } finally {
        rmSync(workspaceRoot, { recursive: true, force: true });
      }
    },
  );
});

describe('support status presentation', () => {
  it('resolves every documented surface against the reviewed catalog', () => {
    const catalog = reviewedCatalog();
    for (const id of TOOL_ORDER) {
      const subject = TOOLS[id]?.supportSubject;
      if (!subject) continue;
      // A surface that names a subject must name one the catalog classifies,
      // or the docs hub would render a card with a silently missing badge.
      expect(statusOf(catalog, subject)).toBeDefined();
    }
  });

  it('reflects the reviewed language-surface decisions', () => {
    const catalog = reviewedCatalog();
    const statusFor = (toolId: string) => {
      const subject = TOOLS[toolId]?.supportSubject;
      return subject ? statusOf(catalog, subject) : undefined;
    };
    expect(statusFor('ts')).toBe('stable');
    expect(statusFor('go')).toBe('stable');
    expect(statusFor('py')).toBe('experimental');
    expect(statusFor('tooling')).toBe('stable');
  });

  it('classifies nothing for the managed platform surface', () => {
    // The support protocol classifies packages, protocols, and features — not
    // hosted services. The card renders no badge rather than inventing one.
    expect(TOOLS['cloud']?.supportSubject).toBeNull();
  });

  specTest(
    'renders a colour and label for every status in the closed vocabulary',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'closed-status-vocabulary',
      check: 'closed-statuses-have-published-labels',
    },
    () => {
      for (const status of ['stable', 'preview', 'experimental'] as const) {
        const meta = statusMeta(status);
        expect(meta.label).toBe(statusLabel(status));
        expect(meta.color).toStartWith('var(--color-');
      }
    },
  );
});

describe('support page rendering', () => {
  specTest(
    'renders every reviewed entry exactly once, under its kind',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'catalog-is-the-only-source',
      check: 'published-page-matches-reviewed-catalog',
    },
    () => {
      const catalog = reviewedCatalog();
      const published = publishedSupportRows(renderSupportPage(catalog))
        .map((row) => `${row.kind}\0${row.id}\0${row.status}`)
        .sort();
      const reviewed = catalog.entries
        .map((entry) => `${entry.kind}\0${entry.id}\0${statusLabel(entry.status)}`)
        .sort();
      expect(published).toEqual(reviewed);
    },
  );

  specTest(
    'renders only the independent default and parity claims each subject carries',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'independent-claims-are-rendered',
      check: 'reviewed-independent-claims-only-are-rendered',
    },
    () => {
      const catalog = reviewedCatalog();
      const rows = publishedSupportRows(renderSupportPage(catalog));
      expect(rows.length).toBe(catalog.entries.length);
      for (const entry of catalog.entries) {
        const row = rows.find((candidate) => candidate.kind === entry.kind && candidate.id === entry.id);
        expect(row).toBeDefined();
        expect(row?.claims).toBe(expectedClaims(entry));
      }
    },
  );

  specTest(
    'separates support status from feature maturity',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'support-is-not-maturity',
      check: 'support-page-separates-status-from-maturity',
    },
    () => {
      const page = renderSupportPage(reviewedCatalog());
      expect(page).toContain('Support status and feature maturity are different concepts');
      expect(page).toContain('/docs/how-to/write-a-feature-spec');
    },
  );

  specTest(
    'states that an unlisted subject has no reviewed status',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'unclassified-subject-shows-no-status',
      check: 'unlisted-subject-disclaimer-is-published',
    },
    () => {
      const page = renderSupportPage(reviewedCatalog());
      expect(page).toContain('no reviewed public support');
      expect(page).toContain('absence is never an implicit');
    },
  );

  it('states that deployed sites are deliberately not classified', () => {
    const page = renderSupportPage(reviewedCatalog());
    expect(page).toContain('Deployed websites are not classified');
  });

  it('links release planning and the license to their canonical sources', () => {
    const page = renderSupportPage(reviewedCatalog());
    expect(page).toContain('blob/main/RELEASE.md');
    expect(page).toContain('blob/main/LICENSE.md');
    // The page links those decisions; it must not restate them.
    expect(page).not.toContain('MIT License');
    expect(page).not.toContain('v1.0.0 under');
  });

  specTest(
    'is a pure function of the catalog, so repeated builds write identical bytes',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'generated-page-is-deterministic',
      check: 'repeated-rendering-writes-identical-bytes',
    },
    () => {
      const catalog = reviewedCatalog();
      expect(renderSupportPage(catalog)).toBe(renderSupportPage(catalog));
    },
  );

  specTest(
    'sorts entries by id within each kind so row order cannot drift',
    {
      feature: 'putnami-dev/published-support-catalog',
      requirement: 'generated-page-is-deterministic',
      check: 'support-rows-have-stable-order',
    },
    () => {
      const shuffled: SupportCatalog = {
        protocolVersion: 1,
        entries: [
          { id: 'z-package', kind: 'package', status: 'stable' },
          { id: 'b-protocol', kind: 'protocol', status: 'preview' },
          { id: 'z-feature', kind: 'feature', status: 'experimental' },
          { id: 'a-package', kind: 'package', status: 'preview' },
          { id: 'a-protocol', kind: 'protocol', status: 'stable' },
          { id: 'a-feature', kind: 'feature', status: 'stable' },
        ],
      };
      const rows = publishedSupportRows(renderSupportPage(shuffled));
      for (const kind of ['package', 'protocol', 'feature'] as const) {
        const ids = rows.filter((row) => row.kind === kind).map((row) => row.id);
        expect(ids).toEqual([...ids].sort((a, b) => a.localeCompare(b)));
      }
    },
  );
});
