import { describe, expect, it } from 'bun:test';
import {
  EVENT_PAYLOAD_ENCODINGS,
  EVENT_SERVER_DEFAULT_ENDPOINTS,
  EVENT_SERVER_DISCOVERY_PATH,
  EVENT_SERVER_TRANSPORTS,
  PUTNAMI_EVENTS_PROTOCOL,
  isCompatibleEventServer,
  isPutnamiEventServerCapabilities,
  type EventServerCapabilities,
} from '../src/protocol';

describe('events protocol', () => {
  const capabilities: EventServerCapabilities = {
    protocol: PUTNAMI_EVENTS_PROTOCOL,
    transports: [EVENT_SERVER_TRANSPORTS.sse, EVENT_SERVER_TRANSPORTS.websocket],
    features: {
      replay: true,
      publish: false,
      ack: false,
      cursor: ['latest', 'earliest', 'message-id'],
      payload: [EVENT_PAYLOAD_ENCODINGS.json],
    },
    endpoints: {
      sse: { method: 'GET', path: EVENT_SERVER_DEFAULT_ENDPOINTS.sse },
      websocket: { method: 'GET', path: EVENT_SERVER_DEFAULT_ENDPOINTS.websocket },
    },
  };

  it('exports the Event Server discovery contract', () => {
    expect(EVENT_SERVER_DISCOVERY_PATH).toBe('/.well-known/putnami/events');
    expect(EVENT_SERVER_DEFAULT_ENDPOINTS.grpcService).toBe('putnami.events.v1.EventServer');
  });

  it('detects compatible Event Server capabilities', () => {
    expect(isPutnamiEventServerCapabilities(capabilities)).toBe(true);
    expect(isCompatibleEventServer(capabilities, { transports: ['sse'], replay: true })).toBe(true);
    expect(isCompatibleEventServer(capabilities, { transports: ['grpc'] })).toBe(false);
    expect(isCompatibleEventServer(capabilities, { publish: true })).toBe(false);
  });
});
