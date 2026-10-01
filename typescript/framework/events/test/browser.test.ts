import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';

describe('browser entrypoint', () => {
  specTest(
    'exports client-safe topic and websocket APIs',
    {
      feature: 'typescript/event-messaging',
      requirement: 'browser-surface',
      check: 'the-browser-entry-exposes-only-topic-client-and-protocol',
    },
    async () => {
      const browserModule = await import('../src/index.browser');

      expect(typeof browserModule.topic).toBe('function');
      expect(typeof browserModule.eventClient).toBe('function');
      expect(browserModule.PUTNAMI_EVENTS_PROTOCOL).toBe('putnami.events.v1');
      expect(browserModule.EVENT_SERVER_DISCOVERY_PATH).toBe('/.well-known/putnami/events');
      expect(typeof browserModule.Uuid).toBe('object');
      expect('events' in browserModule).toBe(false);
      expect('redisStreamTransport' in browserModule).toBe(false);
    },
  );

  specTest(
    'uses the browser-safe root entrypoint for React Native',
    {
      feature: 'typescript/event-messaging',
      requirement: 'browser-surface',
      check: 'the-browser-entry-is-the-react-native-entry-too',
    },
    async () => {
      const pkg = (await Bun.file(new URL('../package.json', import.meta.url)).json()) as {
        exports: { '.': Record<string, string> };
      };

      expect(pkg.exports['.']['react-native']).toBe('./src/index.browser.ts');
    },
  );
});
