import type { HandlerDefinition } from '../handler/handler';
import type { Message, PublishOptions } from '../topic/message';
import type { Envelope, Transport } from './transport';

export type TransportTarget = string | readonly string[];
export type TopicMatch = string | RegExp | ((topic: string) => boolean);

export type TransportRoute =
  | {
      /** Logical topic channel. Takes precedence over topic name matches. */
      channel: string;
      match?: never;
      transport: TransportTarget;
    }
  | {
      /** Topic name match. Strings support `*` wildcards. */
      match: TopicMatch;
      channel?: never;
      transport: TransportTarget;
    };

export interface RoutingTransportConfig {
  /** Named physical transports. */
  transports: Record<string, Transport>;
  /** Ordered routes. Channel routes are evaluated before match routes. */
  routes?: readonly TransportRoute[];
  /** Fallback transport when no route matches. Omit to fail on unmatched topics. */
  defaultTransport?: TransportTarget;
}

export class RoutingTransport implements Transport {
  private readonly transports: Record<string, Transport>;
  private readonly routes: readonly TransportRoute[];
  private readonly defaultTransport: TransportTarget | undefined;
  private readonly startedTransports: Transport[];

  constructor(config: RoutingTransportConfig) {
    this.transports = config.transports;
    this.routes = config.routes ?? [];
    this.defaultTransport = config.defaultTransport;
    this.startedTransports = uniqueTransports(Object.values(this.transports));
    this.validate();
  }

  setScopeFactory(factory: () => Promise<unknown>): void {
    for (const transport of this.startedTransports) {
      if ('setScopeFactory' in transport && typeof transport.setScopeFactory === 'function') {
        transport.setScopeFactory(factory);
      }
    }
  }

  async publish(topic: string, envelope: Envelope, options?: PublishOptions): Promise<void> {
    const transports = this.resolveTransports(topic, envelope.channel);
    await Promise.all(transports.map((transport) => transport.publish(topic, envelope, options)));
  }

  async subscribe(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    const transports = this.resolveTransports(definition.topic.name, definition.topic.channel);
    await Promise.all(transports.map((transport) => transport.subscribe(definition, callback)));
  }

  async start(): Promise<void> {
    await Promise.all(this.startedTransports.map((transport) => transport.start()));
  }

  async stop(): Promise<void> {
    await Promise.allSettled(this.startedTransports.map((transport) => transport.stop()));
  }

  private resolveTransports(topic: string, channel: string | undefined): Transport[] {
    const target = this.resolveTarget(topic, channel);
    if (!target) {
      throw new Error(`No event transport route matched topic '${topic}'.`);
    }

    return uniqueTransports(this.resolveTargetNames(target).map((name) => this.transports[name]));
  }

  private resolveTarget(topic: string, channel: string | undefined): TransportTarget | undefined {
    if (channel) {
      for (const route of this.routes) {
        if ('channel' in route && route.channel === channel) {
          return route.transport;
        }
      }
    }

    for (const route of this.routes) {
      if (isMatchRoute(route) && matchesTopic(route.match, topic)) {
        return route.transport;
      }
    }

    return this.defaultTransport;
  }

  private validate(): void {
    const names = Object.keys(this.transports);
    if (names.length === 0) {
      throw new Error('Routing transport requires at least one named transport.');
    }

    for (const route of this.routes) {
      const hasChannel = isChannelRoute(route);
      const hasMatch = isMatchRoute(route);
      if (hasChannel === hasMatch) {
        throw new Error('Routing transport routes require exactly one of channel or match.');
      }
      if (isChannelRoute(route) && route.channel.length === 0) {
        throw new Error('Routing transport channel routes require a non-empty channel.');
      }
      if (isMatchRoute(route) && typeof route.match === 'string' && route.match.length === 0) {
        throw new Error('Routing transport match routes require a non-empty match pattern.');
      }
      for (const name of this.resolveTargetNames(route.transport)) {
        this.assertTransportName(name);
      }
    }

    for (const name of this.resolveTargetNames(this.defaultTransport)) {
      this.assertTransportName(name);
    }
  }

  private resolveTargetNames(target: TransportTarget | undefined): string[] {
    if (!target) {
      return [];
    }
    if (isTransportTargetList(target)) {
      if (target.length === 0) {
        throw new Error('Routing transport target arrays must not be empty.');
      }
      return [...new Set(target)];
    }
    return [target];
  }

  private assertTransportName(name: string): void {
    if (!this.transports[name]) {
      throw new Error(`Unknown event transport '${name}'.`);
    }
  }
}

export function routingTransport(config: RoutingTransportConfig): RoutingTransport {
  return new RoutingTransport(config);
}

function uniqueTransports(transports: Transport[]): Transport[] {
  return [...new Set(transports)];
}

function isChannelRoute(route: TransportRoute): route is Extract<TransportRoute, { channel: string }> {
  return 'channel' in route && route.channel !== undefined;
}

function isMatchRoute(route: TransportRoute): route is Extract<TransportRoute, { match: TopicMatch }> {
  return 'match' in route && route.match !== undefined;
}

function isTransportTargetList(target: TransportTarget): target is readonly string[] {
  return Array.isArray(target);
}

function matchesTopic(match: TopicMatch, topic: string): boolean {
  if (typeof match === 'function') {
    return match(topic);
  }
  if (match instanceof RegExp) {
    match.lastIndex = 0;
    return match.test(topic);
  }
  if (!match.includes('*')) {
    return match === topic;
  }
  return wildcardToRegExp(match).test(topic);
}

function wildcardToRegExp(pattern: string): RegExp {
  let source = '^';
  for (const char of pattern) {
    source += char === '*' ? '.*' : escapeRegExp(char);
  }
  source += '$';
  return new RegExp(source);
}

function escapeRegExp(char: string): string {
  return /[\\^$.*+?()[\]{}|]/.test(char) ? `\\${char}` : char;
}
