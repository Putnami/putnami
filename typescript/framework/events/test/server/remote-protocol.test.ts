import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { sanitizeRemoteEnvelope } from '../../src/server/remote-protocol';
import { topic } from '../../src/topic/topic';
import { buildEnvelope } from '../../src/transport/transport';

const TestTopic = topic('test.remote.protocol', { id: Uuid, value: String });

function envelopeWithAttributes(attributes: Record<string, unknown>) {
  const envelope = buildEnvelope(TestTopic, { id: crypto.randomUUID(), value: 'x' });
  (envelope as { attributes: Record<string, unknown> }).attributes = attributes;
  return envelope;
}

describe('sanitizeRemoteEnvelope', () => {
  it('strips every caller-supplied auth.* attribute (anti-spoofing)', () => {
    const envelope = envelopeWithAttributes({
      'auth.sub': 'attacker',
      'auth.email': 'evil@example.com',
      'auth.azp': 'evil-app',
      'auth.client_id': 'evil-client',
      region: 'eu',
    });

    sanitizeRemoteEnvelope(envelope);

    // The shared bearer token cannot vouch for end-user identity, so a forged
    // auth.* must never survive to a downstream handler.
    expect(envelope.attributes['auth.sub']).toBeUndefined();
    expect(envelope.attributes['auth.email']).toBeUndefined();
    expect(envelope.attributes['auth.azp']).toBeUndefined();
    expect(envelope.attributes['auth.client_id']).toBeUndefined();
    // Non-reserved attributes are preserved.
    expect(envelope.attributes.region).toBe('eu');
  });

  it('drops non-string attribute values (untrusted wire shape)', () => {
    const envelope = envelopeWithAttributes({
      region: 'eu',
      count: 7,
      nested: { a: 1 },
      flag: true,
    });

    sanitizeRemoteEnvelope(envelope);

    expect(envelope.attributes.region).toBe('eu');
    expect(envelope.attributes.count).toBeUndefined();
    expect(envelope.attributes.nested).toBeUndefined();
    expect(envelope.attributes.flag).toBeUndefined();
  });

  it('mutates and returns the same envelope with a clean string-only bag', () => {
    const envelope = envelopeWithAttributes({ 'auth.sub': 'x', keep: 'yes' });

    const result = sanitizeRemoteEnvelope(envelope);

    expect(result).toBe(envelope);
    expect(Object.keys(result.attributes)).toEqual(['keep']);
  });

  it('leaves a clean envelope unchanged', () => {
    const envelope = envelopeWithAttributes({ region: 'eu', tenant: 'acme' });

    sanitizeRemoteEnvelope(envelope);

    expect(envelope.attributes).toEqual({ region: 'eu', tenant: 'acme' });
  });
});
