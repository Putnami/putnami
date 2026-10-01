import { createExceptionClass } from './error.utils';
import { HttpStatus } from './http-status.enum';

// 4xx Client Errors
export const BadRequestException = createExceptionClass('BadRequestException', HttpStatus.BAD_REQUEST, 'Bad Request');
export const UnauthorizedException = createExceptionClass(
  'UnauthorizedException',
  HttpStatus.UNAUTHORIZED,
  'Unauthorized',
);
export const ForbiddenException = createExceptionClass('ForbiddenException', HttpStatus.FORBIDDEN, 'Forbidden');
export const NotFoundException = createExceptionClass('NotFoundException', HttpStatus.NOT_FOUND, 'Not Found');
export const MethodNotAllowedException = createExceptionClass(
  'MethodNotAllowedException',
  HttpStatus.METHOD_NOT_ALLOWED,
  'Method Not Allowed',
);
export const NotAcceptableException = createExceptionClass(
  'NotAcceptableException',
  HttpStatus.NOT_ACCEPTABLE,
  'Not Acceptable',
);
export const ProxyAuthenticationRequiredException = createExceptionClass(
  'ProxyAuthenticationRequiredException',
  HttpStatus.PROXY_AUTHENTICATION_REQUIRED,
  'Proxy Authentication Required',
);
export const RequestTimeoutException = createExceptionClass(
  'RequestTimeoutException',
  HttpStatus.REQUEST_TIMEOUT,
  'Request Timeout',
);
export const ConflictException = createExceptionClass('ConflictException', HttpStatus.CONFLICT, 'Conflict');
export const GoneException = createExceptionClass('GoneException', HttpStatus.GONE, 'Gone');
export const LengthRequiredException = createExceptionClass(
  'LengthRequiredException',
  HttpStatus.LENGTH_REQUIRED,
  'Length Required',
);
export const PreconditionFailedException = createExceptionClass(
  'PreconditionFailedException',
  HttpStatus.PRECONDITION_FAILED,
  'Precondition Failed',
);
export const PayloadTooLargeException = createExceptionClass(
  'PayloadTooLargeException',
  HttpStatus.PAYLOAD_TOO_LARGE,
  'Payload Too Large',
);
export const UriTooLongException = createExceptionClass('UriTooLongException', HttpStatus.URI_TOO_LONG, 'URI Too Long');
export const UnsupportedMediaTypeException = createExceptionClass(
  'UnsupportedMediaTypeException',
  HttpStatus.UNSUPPORTED_MEDIA_TYPE,
  'Unsupported Media Type',
);
export const RangeNotSatisfiableException = createExceptionClass(
  'RangeNotSatisfiableException',
  HttpStatus.REQUESTED_RANGE_NOT_SATISFIABLE,
  'Requested Range Not Satisfiable',
);
export const ExpectationFailedException = createExceptionClass(
  'ExpectationFailedException',
  HttpStatus.EXPECTATION_FAILED,
  'Expectation Failed',
);
export const ImATeapotException = createExceptionClass('ImATeapotException', HttpStatus.I_AM_A_TEAPOT, "I'm a teapot");
export const MisdirectedException = createExceptionClass('MisdirectedException', HttpStatus.MISDIRECTED, 'Misdirected');
export const UnprocessableEntityException = createExceptionClass(
  'UnprocessableEntityException',
  HttpStatus.UNPROCESSABLE_ENTITY,
  'Unprocessable Entity',
);
export const FailedDependencyException = createExceptionClass(
  'FailedDependencyException',
  HttpStatus.FAILED_DEPENDENCY,
  'Failed Dependency',
);
export const TooManyRequestsException = createExceptionClass(
  'TooManyRequestsException',
  HttpStatus.TOO_MANY_REQUESTS,
  'Too Many Requests',
);

// 5xx Server Errors
export const InternalServerErrorException = createExceptionClass(
  'InternalServerErrorException',
  HttpStatus.INTERNAL_SERVER_ERROR,
  'Internal Server Error',
);
export const NotImplementedException = createExceptionClass(
  'NotImplementedException',
  HttpStatus.NOT_IMPLEMENTED,
  'Not Implemented',
);
export const BadGatewayException = createExceptionClass('BadGatewayException', HttpStatus.BAD_GATEWAY, 'Bad Gateway');
export const ServiceUnavailableException = createExceptionClass(
  'ServiceUnavailableException',
  HttpStatus.SERVICE_UNAVAILABLE,
  'Service Unavailable',
);
export const GatewayTimeoutException = createExceptionClass(
  'GatewayTimeoutException',
  HttpStatus.GATEWAY_TIMEOUT,
  'Gateway Timeout',
);
export const HttpVersionNotSupportedException = createExceptionClass(
  'HttpVersionNotSupportedException',
  HttpStatus.HTTP_VERSION_NOT_SUPPORTED,
  'HTTP Version Not Supported',
);
