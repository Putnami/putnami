import { BadRequestException } from '@putnami/runtime';
import {
  type SchemaDefinition,
  type ValidateOptions,
  type ValidationError,
  validateSchema as validateSchemaCore,
} from '@putnami/runtime';

export type { ValidationError };

/**
 * Validate a plain object against a SchemaDefinition.
 * Throws `BadRequestException` with structured errors on failure.
 *
 * This is the HTTP-aware wrapper around the core `validateSchema` from runtime.
 */
export function validateSchema<S extends SchemaDefinition>(
  schema: S,
  value: unknown,
  options: ValidateOptions = {},
): Record<string, unknown> {
  const { data, errors } = validateSchemaCore(schema, value, options);

  if (errors.length > 0) {
    const message = formatErrors(errors);
    throw new BadRequestException({
      statusCode: 400,
      message,
      error: 'Bad Request',
      errors,
    });
  }

  return data;
}

function formatErrors(errors: ValidationError[]): string {
  return errors.map((e) => e.message).join('; ');
}
