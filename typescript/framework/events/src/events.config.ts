import { ArrayOf, Config, Desc, Int, OneOf, Optional, useConfig } from '@putnami/runtime';
import type { EventsConfig } from './events.plugin';
import type { PushConfig } from './server/push-receiver';

/**
 * Deploy-time events configuration contributed by the framework plugin.
 * Function-valued credential and verifier hooks remain code-owned and are
 * merged back after the document is resolved.
 */
export const EventsRuntimeConfig = Config('events', {
  delivery: Optional(OneOf('pull', 'stream', 'push')),
  transport: Optional(String),
  eventServer: Optional(
    Desc('Managed Event Server publisher binding', {
      contractVersion: Optional(Int),
      endpoint: Optional(String),
      audience: Optional(String),
      protocol: Optional(String),
      workspaceId: Optional(String),
      environment: Optional(String),
      workload: Optional(String),
      topologyGenerationId: Optional(String),
    }),
  ),
  push: Optional(
    Desc('Provider push receiver binding', {
      enabled: Optional(Boolean),
      issuer: Optional(String),
      audience: Optional(String),
      allowedServiceAccounts: Optional(ArrayOf(String)),
    }),
  ),
});

/** Resolve document-owned values over programmatic defaults without serializing code hooks. */
export function resolveEventsRuntimeConfig(config: EventsConfig): EventsConfig {
  const document = useConfig(EventsRuntimeConfig);
  const resolved: EventsConfig = { ...config };

  if (document.delivery) {
    resolved.delivery = document.delivery;
  }

  // An explicit Transport object or named routing table remains authoritative.
  // A string selector is configuration and may be replaced by the resolved
  // managed document.
  if (!config.transports && typeof config.transport !== 'object' && document.transport) {
    resolved.transport = document.transport as EventsConfig['transport'];
  }

  if (document.eventServer || config.eventServer) {
    resolved.eventServer = {
      ...(config.eventServer ?? {}),
      ...(document.eventServer ?? {}),
      tokenSource: config.eventServer?.tokenSource,
    };
  }

  if (document.push || config.push) {
    resolved.push = {
      ...(config.push ?? {}),
      ...(document.push ?? {}),
      verify: config.push?.verify,
    } as PushConfig;
  }

  return resolved;
}
