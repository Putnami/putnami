export const PUTNAMI_EVENTS_PROTOCOL = 'putnami.events.v1' as const;

/** Managed Event Server admission/config compatibility version. */
export const MANAGED_PUBLISH_CONTRACT_VERSION = 1 as const;

/** Exact managed identity/routing attributes callers must never supply. */
export const MANAGED_PUBLISH_RESERVED_ATTRIBUTES = [
  'workspace_id',
  'environment',
  'workload',
  'service',
  'channel',
  'topology_generation_id',
  'traceparent',
  'tracestate',
] as const;

export const EVENT_FRAME_TYPES = {
  auth: 'auth',
  subscribe: 'subscribe',
  publish: 'publish',
  event: 'event',
  ack: 'ack',
  nack: 'nack',
  error: 'error',
  complete: 'complete',
} as const;

export const EVENT_SERVER_TRANSPORTS = {
  sse: 'sse',
  websocket: 'websocket',
  http: 'http',
  grpc: 'grpc',
} as const;

export const EVENT_AUTH_SCHEMES = {
  none: 'none',
  bearer: 'bearer',
  headers: 'headers',
  cookie: 'cookie',
  serviceAccount: 'serviceAccount',
  workloadIdentity: 'workloadIdentity',
} as const;

export const EVENT_CURSOR_MODES = {
  latest: 'latest',
  earliest: 'earliest',
  messageId: 'message-id',
  brokerCursor: 'broker-cursor',
} as const;

export const EVENT_PAYLOAD_ENCODINGS = {
  json: 'json',
} as const;

export const EVENT_SERVER_ERROR_CODES = {
  unauthorized: 'unauthorized',
  forbidden: 'forbidden',
  protocolMismatch: 'protocol_mismatch',
  invalidFrame: 'invalid_frame',
  invalidEnvelope: 'invalid_envelope',
  invalidTopic: 'invalid_topic',
  invalidCursor: 'invalid_cursor',
  unsupportedFeature: 'unsupported_feature',
  payloadTooLarge: 'payload_too_large',
  rateLimited: 'rate_limited',
  upstreamUnavailable: 'upstream_unavailable',
  internal: 'internal',
} as const;

export const EVENT_SERVER_DISCOVERY_PATH = '/.well-known/putnami/events' as const;
export const EVENT_SERVER_DEFAULT_ENDPOINTS = {
  sse: '/events/stream',
  websocket: '/events/ws',
  publish: '/events/publish',
  health: '/events/health',
  grpcService: 'putnami.events.v1.EventServer',
} as const;

export type EventProtocol = typeof PUTNAMI_EVENTS_PROTOCOL;
export type EventFrameType = (typeof EVENT_FRAME_TYPES)[keyof typeof EVENT_FRAME_TYPES];
export type EventServerTransport = (typeof EVENT_SERVER_TRANSPORTS)[keyof typeof EVENT_SERVER_TRANSPORTS];
export type EventAuthScheme = (typeof EVENT_AUTH_SCHEMES)[keyof typeof EVENT_AUTH_SCHEMES];
export type EventCursorMode = (typeof EVENT_CURSOR_MODES)[keyof typeof EVENT_CURSOR_MODES];
export type EventPayloadEncoding = (typeof EVENT_PAYLOAD_ENCODINGS)[keyof typeof EVENT_PAYLOAD_ENCODINGS];
export type EventServerErrorCode = (typeof EVENT_SERVER_ERROR_CODES)[keyof typeof EVENT_SERVER_ERROR_CODES];

export interface EventEnvelope {
  protocol?: EventProtocol;
  id: string;
  topic: string;
  channel?: string;
  payload: unknown;
  key?: string;
  dedupeKey?: string;
  topicVersion?: string;
  timestamp: string;
  attributes: Record<string, string>;
  attempt: number;
  traceId?: string;
}

export interface EventAuthFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.auth;
  token?: string;
  headers?: Record<string, string>;
}

export interface EventSubscribeFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.subscribe;
  topic: string;
  channel?: string;
  from?: 'latest' | 'earliest' | string;
  attributes?: Record<string, string>;
}

export interface EventPublishFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.publish;
  /** Optional in the general v1 profile; required by the managed-workload profile. */
  id?: string;
  topic: string;
  channel?: string;
  payload: unknown;
  key?: string;
  dedupeKey?: string;
  topicVersion?: string;
  attributes?: Record<string, string>;
  traceId?: string;
}

/**
 * Strict managed-workload profile of EventPublishFrame. Managed admission
 * stamps channel and reserved identity/routing attributes.
 */
export interface ManagedPublishFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.publish;
  id: string;
  dedupeKey: string;
  topic: string;
  topicVersion: string;
  payload: unknown;
  key?: string;
  attributes?: Record<string, string>;
  traceId?: string;
}

export interface EventMessageFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.event;
  id: string;
  topic: string;
  channel?: string;
  payload: unknown;
  key?: string;
  dedupeKey?: string;
  topicVersion?: string;
  timestamp?: string;
  attributes?: Record<string, string>;
  traceId?: string;
}

export interface EventAckFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.ack;
  id: string;
}

export interface EventNackFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.nack;
  id: string;
  error?: string;
}

export interface EventErrorFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.error;
  message?: string;
  error?: string;
}

export interface EventCompleteFrame {
  protocol: EventProtocol;
  type: typeof EVENT_FRAME_TYPES.complete;
}

export type EventServerFrame =
  | EventAuthFrame
  | EventSubscribeFrame
  | EventPublishFrame
  | EventMessageFrame
  | EventAckFrame
  | EventNackFrame
  | EventErrorFrame
  | EventCompleteFrame;

export interface EventServerEndpoint {
  method?: 'GET' | 'POST';
  path?: string;
  service?: string;
}

export interface EventServerEndpoints {
  sse?: EventServerEndpoint;
  websocket?: EventServerEndpoint;
  publish?: EventServerEndpoint;
  health?: EventServerEndpoint;
  grpc?: EventServerEndpoint;
}

export interface EventServerFeatures {
  replay: boolean;
  publish: boolean;
  ack: boolean;
  auth?: readonly EventAuthScheme[];
  cursor?: readonly EventCursorMode[];
  payload: readonly EventPayloadEncoding[];
}

export interface EventServerLimits {
  maxPayloadBytes?: number;
  maxFrameBytes?: number;
  maxSubscriptions?: number;
  maxTopicsPerSubscription?: number;
}

export interface EventServerInfo {
  name?: string;
  version?: string;
}

export interface EventServerCapabilities {
  protocol: EventProtocol;
  server?: EventServerInfo;
  transports: readonly EventServerTransport[];
  features: EventServerFeatures;
  endpoints: EventServerEndpoints;
  limits?: EventServerLimits;
}

export interface EventServerError {
  protocol: EventProtocol;
  code: EventServerErrorCode;
  message: string;
  retryable?: boolean;
  details?: Record<string, string>;
}

/** Canonical successful HTTP publish response. */
export interface PublishRef {
  protocol: EventProtocol;
  id: string;
  topic: string;
  timestamp: string;
}

export type ManagedPublishOutcomeClass = 'accepted' | 'permanent' | 'retryable' | 'ambiguous';
export type ManagedPublishOutcomeAction = 'complete' | 'fail' | 'retry';

/** Portable fixture shape under protocols/events/fixtures/managed-publish/v1/outcomes. */
export interface ManagedPublishOutcomeFixture {
  name: string;
  request: { id: string; topic: string };
  input:
    | { http: { status: number; headers?: Record<string, string>; body: unknown } }
    | { transportError: 'timeout' | 'reset' | 'client-canceled-after-send' };
  expected: {
    class: ManagedPublishOutcomeClass;
    action: ManagedPublishOutcomeAction;
    sameRoute?: boolean;
    sameRequestBytes?: boolean;
    honorRetryAfter?: boolean;
  };
}

export interface EventServerCompatibilityRequirements {
  transports?: readonly EventServerTransport[];
  replay?: boolean;
  publish?: boolean;
  ack?: boolean;
}

export function isPutnamiEventsFrame(value: unknown): value is EventServerFrame {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { protocol?: unknown }).protocol === PUTNAMI_EVENTS_PROTOCOL &&
    typeof (value as { type?: unknown }).type === 'string'
  );
}

/** Reports whether value conforms to the strict managed-publish v1 profile. */
export function isManagedPublishFrame(value: unknown): value is ManagedPublishFrame {
  if (typeof value !== 'object' || value === null) {
    return false;
  }

  const frame = value as Record<string, unknown>;
  const allowedFields = new Set([
    'protocol',
    'type',
    'id',
    'dedupeKey',
    'topic',
    'topicVersion',
    'payload',
    'key',
    'attributes',
    'traceId',
  ]);
  if (Object.keys(frame).some((field) => !allowedFields.has(field))) {
    return false;
  }
  if (
    frame['protocol'] !== PUTNAMI_EVENTS_PROTOCOL ||
    frame['type'] !== EVENT_FRAME_TYPES.publish ||
    !isNonBlankString(frame['id']) ||
    !isNonBlankString(frame['dedupeKey']) ||
    !isNonBlankString(frame['topic']) ||
    !isNonBlankString(frame['topicVersion']) ||
    !Object.hasOwn(frame, 'payload') ||
    !isJSONValue(frame['payload'])
  ) {
    return false;
  }
  if (frame['topic'].startsWith('projects/') && frame['topic'].includes('/topics/')) {
    return false;
  }
  if (frame['key'] !== undefined && typeof frame['key'] !== 'string') {
    return false;
  }
  if (frame['traceId'] !== undefined && typeof frame['traceId'] !== 'string') {
    return false;
  }

  const attributes = frame['attributes'];
  if (attributes === undefined) {
    return true;
  }
  if (typeof attributes !== 'object' || attributes === null || Array.isArray(attributes)) {
    return false;
  }
  return Object.entries(attributes).every(
    ([key, item]) => typeof item === 'string' && !isManagedPublishReservedAttribute(key),
  );
}

/** Reports whether a caller attribute belongs to managed admission. */
export function isManagedPublishReservedAttribute(attribute: string): boolean {
  return (
    (MANAGED_PUBLISH_RESERVED_ATTRIBUTES as readonly string[]).includes(attribute) ||
    attribute.startsWith('auth.') ||
    attribute.startsWith('putnami.')
  );
}

function isNonBlankString(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0;
}

function isJSONValue(value: unknown, ancestors = new Set<object>()): boolean {
  if (value === null || typeof value === 'string' || typeof value === 'boolean') {
    return true;
  }
  if (typeof value === 'number') {
    return Number.isFinite(value);
  }
  if (typeof value !== 'object' || ancestors.has(value)) {
    return false;
  }
  if (
    !Array.isArray(value) &&
    Object.getPrototypeOf(value) !== Object.prototype &&
    Object.getPrototypeOf(value) !== null
  ) {
    return false;
  }

  ancestors.add(value);
  const isValid = Object.values(value).every((item) => isJSONValue(item, ancestors));
  ancestors.delete(value);
  return isValid;
}

export function isPutnamiEventServerCapabilities(value: unknown): value is EventServerCapabilities {
  if (typeof value !== 'object' || value === null) {
    return false;
  }

  const capabilities = value as Partial<EventServerCapabilities>;
  return (
    capabilities.protocol === PUTNAMI_EVENTS_PROTOCOL &&
    Array.isArray(capabilities.transports) &&
    capabilities.transports.length > 0 &&
    typeof capabilities.features === 'object' &&
    capabilities.features !== null &&
    Array.isArray(capabilities.features.payload) &&
    capabilities.features.payload.includes(EVENT_PAYLOAD_ENCODINGS.json) &&
    typeof capabilities.endpoints === 'object' &&
    capabilities.endpoints !== null
  );
}

export function isCompatibleEventServer(
  capabilities: EventServerCapabilities,
  requirements: EventServerCompatibilityRequirements = {},
): boolean {
  if (capabilities.protocol !== PUTNAMI_EVENTS_PROTOCOL) {
    return false;
  }
  if (!capabilities.features.payload.includes(EVENT_PAYLOAD_ENCODINGS.json)) {
    return false;
  }
  for (const transport of requirements.transports ?? []) {
    if (!capabilities.transports.includes(transport)) {
      return false;
    }
  }
  if (requirements.replay === true && !capabilities.features.replay) {
    return false;
  }
  if (requirements.publish === true && !capabilities.features.publish) {
    return false;
  }
  if (requirements.ack === true && !capabilities.features.ack) {
    return false;
  }
  return true;
}
