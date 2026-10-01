import { shouldExposeErrorStack } from './expose-stack';
import { HttpException, type HttpExceptionOptions } from './http.exception';
import { HttpStatus } from './http-status.enum';

/**
 * Exception that aggregates multiple errors into a single exception.
 * Useful for batch processing or parallel operations where multiple failures might occur.
 */
export class AggregateException extends HttpException {
  public errors: Error[];

  constructor(errors: Error[], message = 'Multiple errors occurred', options?: HttpExceptionOptions) {
    super(
      {
        message,
        errors: errors.map((e) => ({
          message: e.message,
          name: e.name,
          code: (e as { code?: unknown }).code,
        })),
      },
      HttpStatus.INTERNAL_SERVER_ERROR,
      options,
    );
    this.errors = errors;
  }

  public override toJSON() {
    const base = super.toJSON();
    return {
      ...base,
      errors: this.errors.map((e) => {
        if (e instanceof HttpException) {
          return e.toJSON();
        }
        return {
          message: e.message,
          name: e.name,
          stack: shouldExposeErrorStack() ? e.stack : undefined,
        };
      }),
    };
  }
}
