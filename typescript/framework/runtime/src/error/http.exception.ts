import { isObject, isString } from '@putnami/utils';
import { createBody, extractDescriptionAndOptionsFrom } from './exception-body';
import { shouldExposeErrorStack } from './expose-stack';
import { HttpStatus } from './http-status.enum';

export interface HttpExceptionOptions {
  cause?: Error;
  description?: string;
}

export interface DescriptionAndOptions {
  description?: string;
  httpExceptionOptions?: HttpExceptionOptions;
}

/**
 * Base class for HTTP exceptions raised by Putnami applications.
 *
 * Pairs an HTTP status code with a response payload. Subclasses for the
 * common status codes (e.g. `BadRequestException`, `NotFoundException`) are
 * derived from this class in `exceptions.ts`. When the error reaches the
 * HTTP layer it is serialized via {@link HttpException.toJSON} into the JSON
 * response body.
 */
export class HttpException extends Error {
  public static from(error: unknown): HttpException {
    if (error instanceof HttpException) {
      return error;
    }

    if (error instanceof Error) {
      // Expose only a generic public message; keep the original error (which may
      // carry DB driver text, file paths, or stack-bearing messages) on `cause`
      // for logs rather than leaking it into the client-facing response body.
      return new InternalServerErrorException('Internal Server Error', { cause: error });
    }

    return new InternalServerErrorException('Unknown Error', { cause: error as Error });
  }

  /**
   * Instantiate a plain HTTP Exception.
   *
   * @example
   * throw new HttpException()
   * throw new HttpException('message', HttpStatus.BAD_REQUEST)
   * throw new HttpException({ reason: 'this can be a human readable reason' }, HttpStatus.BAD_REQUEST)
   * throw new HttpException(new Error('Cause Error'), HttpStatus.BAD_REQUEST)
   * throw new HttpException('custom message', HttpStatus.BAD_REQUEST, {
   *  cause: new Error('Cause Error'),
   * })
   *
   *
   * @usageNotes
   * The constructor arguments define the response payload and the HTTP status code.
   * - The `response` argument (required) sets the error payload. A string is used as
   *  the `message`; an object may carry a `message` and/or a `code`. It can also be an
   *  Error, which is then used as the error [cause](https://nodejs.org/en/blog/release/v16.9.0/#error-cause).
   * - The `status` argument (required) is the HTTP status code.
   * - The `options` argument (optional) supports a `cause`, an alternative way to
   *  specify the error cause: `new HttpException('description', 400, { cause: new Error() })`.
   *
   * Serializing the exception with `toJSON()` produces the JSON response body:
   * - `statusCode`: the HTTP status code.
   * - `message`: the message derived from `response` (or a humanized class name).
   * - `code`: `response.code` when provided, otherwise the exception class name.
   * - `stack`: included only outside production (see {@link shouldExposeErrorStack}).
   *
   * The `status` argument should be a valid HTTP status code; use the `HttpStatus`
   * enum exported from `@putnami/runtime`.
   *
   * @param response string, object describing the error condition or the error cause.
   * @param status HTTP response status code.
   * @param options An object used to add an error cause.
   */
  constructor(
    private readonly response: string | object,
    private readonly status: number,
    private readonly options?: HttpExceptionOptions,
  ) {
    super();
    this.initMessage();
    this.initName();
    this.initCause();
  }

  public override cause: Error | undefined;

  public initCause(): void {
    if (this.options?.cause) {
      this.cause = this.options.cause;

      return;
    }

    if (this.response instanceof Error) {
      this.cause = this.response;
    }
  }

  public initMessage() {
    if (isString(this.response)) {
      this.message = this.response as string;
    } else if (isObject(this.response) && isString((this.response as Record<string, unknown>)['message'])) {
      this.message = (this.response as Record<string, unknown>)['message'] as string;
    } else if (this.constructor?.name) {
      this.message = this.constructor?.name.match(/[A-Z][a-z]+|[0-9]+/g)?.join(' ') || '';
    }
  }

  public initName(): void {
    this.name = this.constructor.name;
  }

  public getResponse(): string | object {
    return this.response;
  }

  public getStatus(): number {
    return this.status;
  }

  public toJSON() {
    return {
      statusCode: this.status,
      message: this.message,
      code: this.code,
      stack: shouldExposeErrorStack() ? this.stack : undefined,
    };
  }

  public get code(): string {
    return (this.response as { code?: string })?.code || this.constructor.name;
  }
}

export function isHttpException(value: unknown): value is HttpException {
  return value instanceof HttpException;
}

export type HttpExceptionConstructor = new (
  objectOrError?: string | object,
  descriptionOrOptions?: string | HttpExceptionOptions,
) => HttpException;

/**
 * Creates a named HttpException subclass for a given HTTP status code.
 * The generated class preserves `constructor.name` and supports `instanceof`.
 */
export function createExceptionClass(
  name: string,
  status: number,
  defaultDescription: string,
): HttpExceptionConstructor {
  // Use computed property name to set the class name dynamically
  const classes = {
    [name]: class extends HttpException {
      constructor(
        objectOrError?: string | object,
        descriptionOrOptions: string | HttpExceptionOptions = defaultDescription,
      ) {
        const { description, httpExceptionOptions } = extractDescriptionAndOptionsFrom(descriptionOrOptions);
        super(createBody(objectOrError, description, status), status, httpExceptionOptions);
      }
    },
  };
  return classes[name] as HttpExceptionConstructor;
}

// Defined here, not in exceptions.ts, so that HttpException.from reaches it
// through a static binding: exceptions.ts imports error.utils.ts, which imports
// this module, and a lazy require() back into exceptions.ts becomes a chunk that
// a compiled executable cannot load. exceptions.ts re-exports it.
export const InternalServerErrorException = createExceptionClass(
  'InternalServerErrorException',
  HttpStatus.INTERNAL_SERVER_ERROR,
  'Internal Server Error',
);
