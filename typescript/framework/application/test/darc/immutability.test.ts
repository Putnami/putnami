import { describe, expect, it } from 'bun:test';
import type { ArchitectureImport } from '../../src/architecture/contract.types';
import { validateArchitectureImport } from '../../src/architecture/contract';
import { ContractError } from '../../src/darc/contract';
import { Projection } from '../../src/darc/projection';
import { Snapshot } from '../../src/darc/snapshot';
import { PROJECTION_CONTRACT } from './fixtures/contracts';

const NOW = new Date('2026-01-01T00:00:00.000Z');

const SNAPSHOT_CONTRACT: ArchitectureImport = {
  id: 'consumer.producer-snapshot.v1',
  version: 1,
  from: { domain: 'producer', export: 'producer.observations.v1' },
  as: 'consumer.producer-observations',
  mode: 'snapshot',
  status: 'active',
  facts: ['observation_batch'],
  transport: { kind: 'file', contract: 'putnami.observations.v1', availability: 'active' },
  consistency: {
    maxStaleness: '1h',
    onMissing: 'fail-closed',
    onStale: 'fail-closed',
    ordering: 'none',
    sourceVersion: 'session_id',
    idempotencyKey: 'task_key',
    lateEvents: 'reject',
  },
  justification: 'The consumer joins an immutable producer state attached under its exact version.',
};

interface MutableValue {
  nested: { label: string };
  capturedAt: Date;
  tags: Map<string, string>;
}

function mutableValue(label: string): MutableValue {
  return {
    nested: { label },
    capturedAt: new Date('2025-12-31T23:59:59.000Z'),
    tags: new Map([['source', label]]),
  };
}

function projectionContract(maxStaleness = '24h'): ArchitectureImport {
  return {
    ...PROJECTION_CONTRACT,
    consistency: {
      ...(PROJECTION_CONTRACT.consistency as NonNullable<ArchitectureImport['consistency']>),
      maxStaleness,
    },
  };
}

function snapshotContract(maxStaleness = '1h'): ArchitectureImport {
  return {
    ...SNAPSHOT_CONTRACT,
    consistency: { ...(SNAPSHOT_CONTRACT.consistency as NonNullable<ArchitectureImport['consistency']>), maxStaleness },
  };
}

describe('DARC value ownership', () => {
  it('keeps Projection storage independent at ingress and every read boundary', () => {
    const projection = new Projection<MutableValue>(projectionContract(), {
      bootstrap: () => [],
      clock: () => NOW,
    });
    const writer = projection.writer('consumer.loader');
    const input = mutableValue('original');
    const observedAt = new Date(NOW);

    writer.apply({ id: 'a', value: input, sourceVersion: '1', observedAt });
    input.nested.label = 'mutated at ingress';
    input.capturedAt.setUTCFullYear(2030);
    input.tags.set('source', 'mutated at ingress');
    observedAt.setUTCFullYear(2030);

    const first = projection.get('a');
    expect(first?.value.nested.label).toBe('original');
    expect(first?.value.capturedAt.toISOString()).toBe('2025-12-31T23:59:59.000Z');
    expect(first?.value.tags.get('source')).toBe('original');
    expect(first?.observedAt.toISOString()).toBe('2026-01-01T00:00:00.000Z');

    (first as NonNullable<typeof first>).value.nested.label = 'mutated from get';
    (first as NonNullable<typeof first>).value.capturedAt.setUTCFullYear(2031);
    (first as NonNullable<typeof first>).observedAt.setUTCFullYear(2031);
    const enumerated = projection.all()[0] as NonNullable<ReturnType<typeof projection.get>>;
    enumerated.value.tags.set('source', 'mutated from all');
    enumerated.observedAt.setUTCFullYear(2032);

    const reread = projection.get('a');
    expect(reread?.value.nested.label).toBe('original');
    expect(reread?.value.capturedAt.toISOString()).toBe('2025-12-31T23:59:59.000Z');
    expect(reread?.value.tags.get('source')).toBe('original');
    expect(reread?.observedAt.toISOString()).toBe('2026-01-01T00:00:00.000Z');
  });

  it('keeps Snapshot versions independent at ingress and every read boundary', () => {
    const snapshot = new Snapshot<MutableValue>(snapshotContract(), { clock: () => NOW });
    const input = mutableValue('original');
    const observedAt = new Date(NOW);

    snapshot.attach('v1', input, observedAt);
    input.nested.label = 'mutated at ingress';
    input.capturedAt.setUTCFullYear(2030);
    input.tags.set('source', 'mutated at ingress');
    observedAt.setUTCFullYear(2030);

    const first = snapshot.at('v1');
    expect(first?.value.nested.label).toBe('original');
    expect(first?.value.capturedAt.toISOString()).toBe('2025-12-31T23:59:59.000Z');
    expect(first?.value.tags.get('source')).toBe('original');
    expect(first?.observedAt.toISOString()).toBe('2026-01-01T00:00:00.000Z');

    (first as NonNullable<typeof first>).value.nested.label = 'mutated from at';
    (first as NonNullable<typeof first>).value.capturedAt.setUTCFullYear(2031);
    (first as NonNullable<typeof first>).observedAt.setUTCFullYear(2031);
    const latest = snapshot.latest() as NonNullable<ReturnType<typeof snapshot.latest>>;
    latest.value.tags.set('source', 'mutated from latest');
    latest.observedAt.setUTCFullYear(2032);

    const reread = snapshot.at('v1');
    expect(reread?.value.nested.label).toBe('original');
    expect(reread?.value.capturedAt.toISOString()).toBe('2025-12-31T23:59:59.000Z');
    expect(reread?.value.tags.get('source')).toBe('original');
    expect(reread?.observedAt.toISOString()).toBe('2026-01-01T00:00:00.000Z');
  });

  it('fails explicitly for unsupported structured-clone values and names the custom option', () => {
    type CallableValue = { label: string; callback: () => string };
    const value: CallableValue = { label: 'callable', callback: () => 'ok' };
    const projection = new Projection<CallableValue>(projectionContract(), { bootstrap: () => [] });
    const snapshot = new Snapshot<CallableValue>(snapshotContract());

    expect(() => projection.writer('consumer.loader').apply({ id: 'a', value, sourceVersion: '1' })).toThrow(
      /ProjectionOptions\.cloneValue/,
    );
    expect(() => snapshot.attach('v1', value)).toThrow(/SnapshotOptions\.cloneValue/);

    const custom = new Snapshot<CallableValue>(snapshotContract(), {
      cloneValue: (candidate) => ({ ...candidate }),
    });
    custom.attach('v1', value);
    expect(custom.at('v1')?.value.callback()).toBe('ok');
  });
});

describe('DARC Date precision', () => {
  it('keeps Go-valid sub-millisecond and fractional-millisecond durations out of Date-backed components', () => {
    for (const maxStaleness of ['1ns', '1.5ms']) {
      const projectionDeclaration = projectionContract(maxStaleness);
      const snapshotDeclaration = snapshotContract(maxStaleness);
      expect(validateArchitectureImport(projectionDeclaration)).toEqual([]);
      expect(validateArchitectureImport(snapshotDeclaration)).toEqual([]);
      expect(() => new Projection(projectionDeclaration, { bootstrap: () => [] })).toThrow(ContractError);
      expect(() => new Snapshot(snapshotDeclaration)).toThrow(ContractError);
    }
  });

  it('accepts the smallest exactly enforceable Date bound', () => {
    expect(() => new Projection(projectionContract('1ms'), { bootstrap: () => [] })).not.toThrow();
    expect(() => new Snapshot(snapshotContract('1ms'))).not.toThrow();
  });
});
