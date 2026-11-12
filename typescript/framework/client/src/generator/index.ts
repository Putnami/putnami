export { ClientGeneratorPlugin, clientGenerator } from './client-generator.plugin';
export {
  CLIENT_IR_VERSION,
  type ContentIR,
  type FieldIR,
  type MethodIR,
  type ParameterIR,
  type RequestIR,
  type ResponseHeaderIR,
  type ServiceIR,
  type SpecIR,
  type SuccessIR,
} from './ir.type';
export {
  ClientGenerationError,
  type ClientGenerationErrorCode,
  ExactJsonNumber,
  readOpenApiSource,
  readOpenApiSpec,
  type ReadOpenApiOptions,
  readOpenApiSpecWithOptions,
  serializeClientIR,
} from './openapi-reader';
export { type GenerateClientOptions, type GenerateClientResult, generateClient } from './project-clients';
export { readProtoSpec } from './proto-reader';
export {
  pageTransportSchemas,
  validatePageTransportSchemas,
  QueryParamRelation,
  QueryParamAfterKey,
  QueryParamLimit,
} from './page';
export { type GeneratedFile, generateTypeScriptClient, type TsGeneratorOptions } from './ts/ts-generator';
