import { describe, expect, it } from 'bun:test';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import {
  PUTNAMI_EVENTS_PROTOCOL,
  isManagedPublishFrame,
  isPutnamiEventServerCapabilities,
  isPutnamiEventsFrame,
  type ManagedPublishOutcomeFixture,
} from '../src/protocol';

// The shared fixture corpus is the cross-language test surface declared canonical
// in protocols/events (see its README and doc/10-protocol.md). The Go
// strict parsers validate the same files in
// protocols/events/conformance_test.go; running them through the
// TypeScript protocol surface here keeps `@putnami/events/protocol` from silently
// drifting away from the canonical schemas and fixtures.
const FIXTURES_DIR = join(__dirname, '../../../../protocols/events/fixtures');
const MANAGED_PUBLISH_V1_DIR = join(FIXTURES_DIR, 'managed-publish', 'v1');

type Fixture = { name: string; data: Record<string, unknown> };

function loadFixtures(kind: 'valid' | 'invalid'): Fixture[] {
  const dir = join(FIXTURES_DIR, kind);
  return readdirSync(dir)
    .filter((name) => name.endsWith('.json'))
    .sort()
    .map((name) => ({
      name,
      data: JSON.parse(readFileSync(join(dir, name), 'utf8')) as Record<string, unknown>,
    }));
}

// Classify a fixture by file name the same way the Go conformance runner does, so
// both languages agree on which shape each fixture exercises.
function shapeOf(name: string): 'envelope' | 'capabilities' | 'error' | 'frame' {
  if (name.includes('envelope')) return 'envelope';
  if (name.includes('capabilities')) return 'capabilities';
  if (name.includes('error')) return 'error';
  return 'frame';
}

describe('events protocol fixture conformance', () => {
  const validFixtures = loadFixtures('valid');
  const invalidFixtures = loadFixtures('invalid');

  it('finds the shared fixture corpus', () => {
    expect(validFixtures.length).toBeGreaterThan(0);
    expect(invalidFixtures.length).toBeGreaterThan(0);
  });

  for (const fixture of validFixtures) {
    it(`accepts valid fixture ${fixture.name}`, () => {
      // Every canonical wire shape carries the protocol identifier.
      expect(fixture.data['protocol']).toBe(PUTNAMI_EVENTS_PROTOCOL);

      switch (shapeOf(fixture.name)) {
        case 'frame':
          expect(isPutnamiEventsFrame(fixture.data)).toBe(true);
          break;
        case 'capabilities':
          expect(isPutnamiEventServerCapabilities(fixture.data)).toBe(true);
          break;
        case 'envelope':
          for (const field of ['id', 'topic', 'payload', 'timestamp', 'attempt']) {
            expect(fixture.data[field]).toBeDefined();
          }
          break;
        case 'error':
          for (const field of ['code', 'message']) {
            expect(fixture.data[field]).toBeDefined();
          }
          break;
      }
    });
  }

  // The TypeScript guards assert protocol identity and structural shape; deep
  // field/enum validation is owned by the canonical JSON Schemas and the Go
  // strict parsers. These cases cover the invariants the guards do enforce.
  it('rejects a frame fixture that omits the protocol identifier', () => {
    const fixture = invalidFixtures.find((f) => f.name === 'gateway-missing-protocol.json');
    expect(fixture).toBeDefined();
    expect(isPutnamiEventsFrame(fixture?.data)).toBe(false);
  });

  it('rejects capabilities advertising a different protocol version', () => {
    const fixture = invalidFixtures.find((f) => f.name === 'gateway-capabilities-bad-protocol.json');
    expect(fixture).toBeDefined();
    expect(isPutnamiEventServerCapabilities(fixture?.data)).toBe(false);
  });
});

describe('managed publish v1 fixture conformance', () => {
  const loadManaged = (kind: 'valid' | 'invalid') =>
    readdirSync(join(MANAGED_PUBLISH_V1_DIR, kind))
      .filter((name) => name.endsWith('.json'))
      .sort()
      .map((name) => ({
        name,
        data: JSON.parse(readFileSync(join(MANAGED_PUBLISH_V1_DIR, kind, name), 'utf8')) as unknown,
      }));

  const validFixtures = loadManaged('valid');
  const invalidFixtures = loadManaged('invalid');

  it('loads the canonical managed corpus without a TypeScript copy', () => {
    expect(validFixtures.length).toBeGreaterThanOrEqual(4);
    expect(invalidFixtures.length).toBeGreaterThanOrEqual(18);
  });

  for (const fixture of validFixtures) {
    it(`accepts managed valid fixture ${fixture.name}`, () => {
      expect(isManagedPublishFrame(fixture.data)).toBe(true);
    });
  }

  for (const fixture of invalidFixtures) {
    it(`rejects managed invalid fixture ${fixture.name}`, () => {
      expect(isManagedPublishFrame(fixture.data)).toBe(false);
    });
  }

  it('rejects payloads that cannot be represented as JSON', () => {
    const frame = (payload: unknown) => ({
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      type: 'publish',
      id: 'evt-json-payload',
      dedupeKey: 'json-payload',
      topic: 'orders.created',
      topicVersion: '1',
      payload,
    });

    const circular: { self?: unknown } = {};
    circular.self = circular;

    expect(isManagedPublishFrame(frame(undefined))).toBe(false);
    expect(isManagedPublishFrame(frame(Number.NaN))).toBe(false);
    expect(isManagedPublishFrame(frame(circular))).toBe(false);
  });

  it('pins byte-identical request bodies across retry attempts', () => {
    const first = readFileSync(join(MANAGED_PUBLISH_V1_DIR, 'valid', 'retry-attempt-1.json'));
    const retry = readFileSync(join(MANAGED_PUBLISH_V1_DIR, 'valid', 'retry-attempt-2.json'));
    expect(retry.equals(first)).toBe(true);
  });

  it('covers accepted, permanent, retryable, and ambiguous outcomes', () => {
    const outcomes = readdirSync(join(MANAGED_PUBLISH_V1_DIR, 'outcomes'))
      .filter((name) => name.endsWith('.json'))
      .sort()
      .map(
        (name) =>
          JSON.parse(
            readFileSync(join(MANAGED_PUBLISH_V1_DIR, 'outcomes', name), 'utf8'),
          ) as ManagedPublishOutcomeFixture,
      );

    expect(new Set(outcomes.map((outcome) => outcome.expected.class))).toEqual(
      new Set(['accepted', 'permanent', 'retryable', 'ambiguous']),
    );
    for (const outcome of outcomes) {
      if (outcome.expected.action === 'retry') {
        expect(outcome.expected.sameRoute).toBe(true);
        expect(outcome.expected.sameRequestBytes).toBe(true);
      }
    }
  });
});
