export { BaseClient, type ClientConfig, type TransportMode } from './base-client';
export {
  CircuitBreaker,
  type CircuitBreakerConfig,
  CircuitOpenError,
  type CircuitState,
  circuitBreakerInterceptor,
} from './circuit-breaker';
export { type BinaryPayload, type BinarySource, type StreamedBinaryPayload, readBoundedBody } from './binary';
export { ClientBuilder } from './client-builder';
export {
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  type StreamHandlers,
  StreamRelay,
  writeEnvelope,
} from './connect-stream';
export {
  type ConnectEncoding,
  type ConnectStreamOptions,
  ConnectTransport,
  type ConnectTransportOptions,
  type ProtoMeta,
  protoMetaFromDescriptor,
} from './connect-transport';
export {
  ClientCredentialError,
  ClientCanceledError,
  ClientDeadlineError,
  ClientError,
  ClientFrameworkError,
  type ClientGrpcStatus,
  ClientRequestError,
  ClientRequestEncodingError,
  ClientResponseContractError,
  ClientResponseSizeError,
  ClientRetryExhaustedError,
  ClientServerError,
  ClientServiceConfigError,
  ClientStreamError,
  ClientTimeoutError,
  ClientTransportUnavailableError,
  isClientFrameworkError,
} from './errors';
export { decodeJsonBody, decodeJsonValue, encodeHttpParameter, encodeJsonBody, parseJsonValue } from './json-codec';
export {
  type ConnectErrorInfo,
  type ConnectStreamTerminal,
  connectErrorInfo,
  formatConnectTimeoutMs,
  grpcCodeToConnectCode,
  GrpcStatus,
  type GrpcStatusCode,
  type GrpcStatusName,
  grpcStatusCode,
  grpcStatusName,
  httpStatusToGrpcCode,
  parseConnectError,
  parseEndStreamTerminal,
} from './grpc-status';
export { HttpTransport } from './http-transport';
export { type SseStreamOptions, SseTransport } from './sse-transport';
export { type SseContinuationOptions, SseDelivery } from './sse-continuation';
export { ServiceWebSocketTransport, type ServiceWebSocketOptions, isWebSocketAvailable } from './service-ws-transport';
export {
  type ByteStream,
  type FrameStream,
  ProviderWebSocketTransport,
  type ProviderWireOptions,
} from './provider-ws-transport';
export {
  StreamSession,
  type StreamAttempt,
  type StreamAttemptResult,
  type StreamBudgets,
  type StreamCallResult,
  type StreamPhase,
  type StreamSessionOptions,
} from './stream-session';
export {
  type ClientFeatureTrace,
  clearGeneratedClientUsages,
  type GeneratedClientDesign,
  type GeneratedClientOperation,
  type GeneratedClientUsage,
  type GeneratedClientUsageSource,
  getGeneratedClientUsages,
} from './generated-client-design';
export { DEFAULT_MAX_RESPONSE_SIZE } from './response-cap';
export {
  CLIENT_RUNTIME_CAPABILITIES,
  DEFAULT_CACHE_MAX_ENTRIES,
  requireClientRuntimeCapabilities,
  type ResponseFieldValue,
  STALE_SERVED_METRIC,
  ServiceResponseCache,
  type ServiceResponseCacheOptions,
  serviceResponseCacheInterceptor,
} from './response-cache';
export {
  type Credential,
  type CredentialBinding,
  CredentialManager,
  type CredentialManagerOptions,
  type CredentialProvider,
  type CredentialRequest,
  type CredentialSourceKind,
  CredentialRegistryClosedError,
} from './credential';
export {
  bindServiceClient,
  createServiceClientRegistration,
  type GeneratedClientConstructor,
  type GeneratedServiceDescriptor,
  registerServiceClient,
  type ServiceBinding,
  type ServiceClientBindingTarget,
} from './service-binding';
export {
  FIRST_PARTY_UNARY_DEFAULTS,
  type ResolvedClientResilience,
  resolveClientResilience,
} from './service-resilience';
export { serviceAttemptTelemetryInterceptor, serviceCallTelemetryInterceptor } from './service-telemetry';
export { ClientResponseValidationError, validateResponseShape } from './response-validator';
export { computeDelay, type RetryConfig, retryInterceptor } from './retry';
export { checkSpecDrift } from './spec-drift';
export { SuccessBody } from './success-body';
export type { DuplexStream, StreamObserver } from './stream.type';
export {
  type ClientRequest,
  type ClientResponse,
  type DisposableInterceptor,
  type Interceptor,
  isDisposableInterceptor,
  type Transport,
} from './transport.type';
export { WebSocketTransport } from './ws-transport';
