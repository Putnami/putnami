import { HttpException } from '@putnami/runtime';

/**
 * Exception thrown by ctx.throw() to abort request processing.
 * Middlewares can catch this to perform cleanup before the response is sent.
 */
export class HttpAbortException extends HttpException {
  constructor(status: number, message?: string) {
    super(message || `HTTP ${status}`, status);
  }
}
