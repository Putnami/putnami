import { isObject, isString, throwError } from '@putnami/utils';
import { HttpException, type DescriptionAndOptions, type HttpExceptionOptions } from './http.exception';

/**
 * Utility method used to extract the error description and httpExceptionOptions from the given argument.
 * This is used by inheriting classes to correctly parse both options.
 * @returns the error description and the httpExceptionOptions as an object.
 */
export function extractDescriptionAndOptionsFrom(
  descriptionOrOptions: string | HttpExceptionOptions,
): DescriptionAndOptions {
  const description = isString(descriptionOrOptions)
    ? (descriptionOrOptions as string)
    : (descriptionOrOptions as HttpExceptionOptions)?.description;

  const httpExceptionOptions = isString(descriptionOrOptions) ? {} : descriptionOrOptions;

  return {
    description,
    httpExceptionOptions: httpExceptionOptions as HttpExceptionOptions,
  };
}

export function createBody(objectOrErrorMessage?: object | string, description?: string, statusCode?: number) {
  if (!objectOrErrorMessage) {
    return { statusCode, message: description };
  }

  return isObject(objectOrErrorMessage) && !Array.isArray(objectOrErrorMessage)
    ? objectOrErrorMessage
    : { statusCode, message: objectOrErrorMessage, error: description };
}

export function getDescriptionFrom(descriptionOrOptions: string | HttpExceptionOptions): string {
  return extractDescriptionAndOptionsFrom(descriptionOrOptions).description || '';
}

export function getHttpExceptionOptionsFrom(descriptionOrOptions: string | HttpExceptionOptions): HttpExceptionOptions {
  return extractDescriptionAndOptionsFrom(descriptionOrOptions).httpExceptionOptions || {};
}

export const error = throwError;

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
