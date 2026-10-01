import { BadRequestException } from './exceptions';

export interface ValidationErrorResponse {
  field: string;
  constraints: string[];
}

export interface ValidationErrorInput {
  property: string;
  constraints?: Record<string, string>;
  children?: ValidationErrorInput[];
}

export class ValidationException extends BadRequestException {
  constructor(errors: ValidationErrorInput[]) {
    const formattedErrors = ValidationException.formatErrors(errors);
    super({ message: 'Validation Failed', errors: formattedErrors }, 'Validation Failed');
  }

  private static formatErrors(errors: ValidationErrorInput[]): ValidationErrorResponse[] {
    const result: ValidationErrorResponse[] = [];
    for (const error of errors) {
      if (error.constraints) {
        result.push({ field: error.property, constraints: Object.values(error.constraints) });
      }
      if (error.children && error.children.length > 0) {
        const childrenErrors = ValidationException.formatErrors(error.children);
        for (const child of childrenErrors) {
          result.push({ field: `${error.property}.${child.field}`, constraints: child.constraints });
        }
      }
    }
    return result;
  }
}
