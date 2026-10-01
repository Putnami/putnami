export { type ClientGeneratorConfig, ClientGeneratorPlugin, clientGenerator } from './client-generator.plugin';
export {
  CLIENTGEN_DEFAULTS,
  type ClientGenConfig,
  type ClientGenConfigContext,
  type ClientGenConfigInput,
  type ClientGenConfigOperation,
  type ClientTarget,
  type GoClientGenConfig,
  resolveClientGenConfig,
  serializeClientGenConfig,
  type TsClientGenConfig,
} from './config.type';
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
export { generateProjectClients, type ProjectClientResult } from './project-clients';
export { readProtoSpec } from './proto-reader';
export {
  pageTransportSchemas,
  validatePageTransportSchemas,
  QueryParamRelation,
  QueryParamAfterKey,
  QueryParamLimit,
} from './page';
export { type GeneratedFile, generateTypeScriptClient, type TsGeneratorOptions } from './ts/ts-generator';
