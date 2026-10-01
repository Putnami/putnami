import { describe, expect, it } from 'bun:test';
import * as eventsModule from '../src/index';

describe('package index', () => {
  it('re-exports the public API surface', () => {
    expect(typeof eventsModule.topic).toBe('function');
    expect(typeof eventsModule.handler).toBe('function');
    expect(typeof eventsModule.getPublisher).toBe('function');
    expect(typeof eventsModule.eventClient).toBe('function');
    expect(eventsModule.PUTNAMI_EVENTS_PROTOCOL).toBe('putnami.events.v1');
    expect(eventsModule.EVENT_SERVER_DISCOVERY_PATH).toBe('/.well-known/putnami/events');
    expect(typeof eventsModule.MemoryBroker).toBe('function');
    expect(typeof eventsModule.MemoryServer).toBe('function');
    expect(typeof eventsModule.events).toBe('function');
    expect(typeof eventsModule.EventsPlugin).toBe('function');
    expect(typeof eventsModule.routingTransport).toBe('function');
    expect(typeof eventsModule.redisPubSub).toBe('function');
    expect(typeof eventsModule.redisStream).toBe('function');
    expect(typeof eventsModule.redisStreamTransport).toBe('function');
    expect(typeof eventsModule.googlePubSubTransport).toBe('function');
    expect(typeof eventsModule.postgresRealtime).toBe('function');
    expect(eventsModule.schema).toBeDefined();
    expect(eventsModule.Uuid).toBeDefined();
  });
});
