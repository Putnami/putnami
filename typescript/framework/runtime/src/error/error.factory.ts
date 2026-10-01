import { HttpException, type HttpExceptionOptions } from './http.exception';
import { HttpStatus } from './http-status.enum';
import * as exceptions from './exceptions';

type ExceptionConstructor = new (
  message?: string | object,
  descriptionOrOptions?: string | HttpExceptionOptions,
) => HttpException;

const exceptionMap: Record<number, ExceptionConstructor> = {
  [HttpStatus.BAD_REQUEST]: exceptions.BadRequestException,
  [HttpStatus.UNAUTHORIZED]: exceptions.UnauthorizedException,
  [HttpStatus.FORBIDDEN]: exceptions.ForbiddenException,
  [HttpStatus.NOT_FOUND]: exceptions.NotFoundException,
  [HttpStatus.METHOD_NOT_ALLOWED]: exceptions.MethodNotAllowedException,
  [HttpStatus.NOT_ACCEPTABLE]: exceptions.NotAcceptableException,
  [HttpStatus.PROXY_AUTHENTICATION_REQUIRED]: exceptions.ProxyAuthenticationRequiredException,
  [HttpStatus.REQUEST_TIMEOUT]: exceptions.RequestTimeoutException,
  [HttpStatus.CONFLICT]: exceptions.ConflictException,
  [HttpStatus.GONE]: exceptions.GoneException,
  [HttpStatus.LENGTH_REQUIRED]: exceptions.LengthRequiredException,
  [HttpStatus.PRECONDITION_FAILED]: exceptions.PreconditionFailedException,
  [HttpStatus.PAYLOAD_TOO_LARGE]: exceptions.PayloadTooLargeException,
  [HttpStatus.URI_TOO_LONG]: exceptions.UriTooLongException,
  [HttpStatus.UNSUPPORTED_MEDIA_TYPE]: exceptions.UnsupportedMediaTypeException,
  [HttpStatus.REQUESTED_RANGE_NOT_SATISFIABLE]: exceptions.RangeNotSatisfiableException,
  [HttpStatus.EXPECTATION_FAILED]: exceptions.ExpectationFailedException,
  [HttpStatus.I_AM_A_TEAPOT]: exceptions.ImATeapotException,
  [HttpStatus.MISDIRECTED]: exceptions.MisdirectedException,
  [HttpStatus.UNPROCESSABLE_ENTITY]: exceptions.UnprocessableEntityException,
  [HttpStatus.FAILED_DEPENDENCY]: exceptions.FailedDependencyException,
  [HttpStatus.TOO_MANY_REQUESTS]: exceptions.TooManyRequestsException,
  [HttpStatus.INTERNAL_SERVER_ERROR]: exceptions.InternalServerErrorException,
  [HttpStatus.NOT_IMPLEMENTED]: exceptions.NotImplementedException,
  [HttpStatus.BAD_GATEWAY]: exceptions.BadGatewayException,
  [HttpStatus.SERVICE_UNAVAILABLE]: exceptions.ServiceUnavailableException,
  [HttpStatus.GATEWAY_TIMEOUT]: exceptions.GatewayTimeoutException,
  [HttpStatus.HTTP_VERSION_NOT_SUPPORTED]: exceptions.HttpVersionNotSupportedException,
};

/**
 * Create an HTTP exception based on the status code.
 * @param status The HTTP status code
 * @param message The error message or object
 * @param descriptionOrOptions The error description or options
 * @returns The corresponding HttpException
 */
export function createHttpException(
  status: number,
  message?: string | object,
  descriptionOrOptions?: string | HttpExceptionOptions,
): HttpException {
  const ExceptionClass = exceptionMap[status];
  if (ExceptionClass) {
    return new ExceptionClass(message, descriptionOrOptions);
  }
  return new HttpException(message || 'Error', status, descriptionOrOptions as HttpExceptionOptions);
}
