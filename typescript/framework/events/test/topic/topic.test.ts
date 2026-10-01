import { describe, expect, it } from 'bun:test';
import { Uuid, Email } from '@putnami/runtime';
import { topic, isTopicDefinition } from '../../src/topic/topic';

describe('topic()', () => {
  it('should create a TopicDefinition with marker', () => {
    const t = topic('user.created', { id: Uuid, email: Email, name: String });

    expect(t.__topic).toBe('putnami:topic');
    expect(t.name).toBe('user.created');
    expect(t.schema).toEqual({ id: Uuid, email: Email, name: String });
  });

  it('should be detected by isTopicDefinition', () => {
    const t = topic('test', { value: String });

    expect(isTopicDefinition(t)).toBe(true);
    expect(isTopicDefinition({})).toBe(false);
    expect(isTopicDefinition(null)).toBe(false);
    expect(isTopicDefinition('string')).toBe(false);
  });

  it('should keep optional topic contract metadata', () => {
    const t = topic(
      'user.created',
      { id: Uuid },
      {
        version: 'v1',
        channel: 'analytics',
        metadata: { owner: 'identity' },
      },
    );

    expect(t.version).toBe('v1');
    expect(t.channel).toBe('analytics');
    expect(t.metadata).toEqual({ owner: 'identity' });
  });
});
