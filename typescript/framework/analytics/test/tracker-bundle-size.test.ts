import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { join } from 'node:path';

const FEATURE = 'typescript/web-analytics-collection';
const BOUNDARY = 'the-browser-bundle-stays-inside-the-boundary';

/** The tracker budget: 4 KiB gzipped, on the critical path of every page. */
const MAX_GZIPPED_BYTES = 4096;

describe('tracker bundle', () => {
  specTest(
    'stays under four kibibytes gzipped',
    { feature: FEATURE, requirement: BOUNDARY, check: 'the-tracker-bundle-is-under-4-kib-gzipped' },
    async () => {
      // The budget is a constraint, not a target: the tag is injected into
      // every server-rendered page, so its weight is paid by every visitor on
      // every first load. A dependency added inside `src/client/**` shows up
      // here before it shows up in someone's Core Web Vitals.
      const built = await Bun.build({
        entrypoints: [join(import.meta.dir, '..', 'src', 'client', 'entry.ts')],
        target: 'browser',
        minify: true,
        format: 'esm',
      });

      if (!built.success) {
        throw new Error(built.logs.map((log) => log.message).join('\n'));
      }

      const sources = await Promise.all(built.outputs.map((output) => output.text()));
      const gzipped = Bun.gzipSync(Buffer.from(sources.join('\n'), 'utf8'));

      expect(gzipped.length).toBeLessThanOrEqual(MAX_GZIPPED_BYTES);
    },
    30_000,
  );
});
