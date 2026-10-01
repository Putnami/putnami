export type { GrpcConfig } from './grpc.plugin';
export { GrpcPlugin, grpc } from './grpc.plugin';
export {
  encodeProto,
  decodeProto,
  encodeVarint,
  enumWireNumber,
  protoEnumRegistry,
  createEnvelope,
  readEnvelope,
  type ProtoEnumRegistry,
  type ProtoEnumValues,
} from './proto-codec';
export * from './connect-protocol';
export { registerReflection } from './grpc-reflection';
// Note: the gRPC test client (`createGrpcTestClient` and its types) lives in the
// `@putnami/application/testing` entry point, not here, so it stays out of the
// production runtime surface.
