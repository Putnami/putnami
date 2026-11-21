import { isObject, isString } from '@putnami/utils';
import type { DescriptionAndOptions, HttpExceptionOptions } from './http.exception';

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
