import { throwError } from '@putnami/utils';
import { extractDescriptionAndOptionsFrom } from './exception-body';
import type { HttpExceptionOptions } from './http.exception';

// The exception-class factory lives in http.exception.ts and the body helpers in
// exception-body.ts, so HttpException.from builds its 500 without importing
// exceptions.ts, which imports this module. They stay exported from here.
export { createBody, extractDescriptionAndOptionsFrom } from './exception-body';
export { createExceptionClass, type HttpExceptionConstructor } from './http.exception';

export function getDescriptionFrom(descriptionOrOptions: string | HttpExceptionOptions): string {
  return extractDescriptionAndOptionsFrom(descriptionOrOptions).description || '';
}

export function getHttpExceptionOptionsFrom(descriptionOrOptions: string | HttpExceptionOptions): HttpExceptionOptions {
  return extractDescriptionAndOptionsFrom(descriptionOrOptions).httpExceptionOptions || {};
}

export const error = throwError;
