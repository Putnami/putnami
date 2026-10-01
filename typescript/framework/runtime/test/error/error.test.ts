import { describe, expect, it } from 'bun:test';
import { restoreEnv } from '@putnami/utils';
import {
  BadRequestException,
  createHttpException,
  getHttpStatusCode,
  getHttpStatusText,
  HttpException,
  isHttpException,
} from '../../src/index';
import {
  createBody,
  extractDescriptionAndOptionsFrom,
  getDescriptionFrom,
  getHttpExceptionOptionsFrom,
} from '../../src/error/error.utils';

describe('Error Module', () => {
  describe('HttpException', () => {
    it('should create an exception with correct status and message', () => {
      const error = new HttpException('Error msg', 400);
      expect(error.getStatus()).toBe(400);
      expect(error.message).toBe('Error msg');
    });

    it('should serialize correctly using toJSON', () => {
      const error = new BadRequestException('Bad thing happened');
      const json = error.toJSON();

      expect(json.statusCode).toBe(400);
      expect(json.message).toBe('Bad thing happened');
      expect(json.code).toBe('BadRequestException');
    });

    it('should omit stack traces in production', () => {
      const original = process.env.NODE_ENV;
      process.env.NODE_ENV = 'production';
      try {
        const error = new BadRequestException('test');
        expect(error.toJSON().stack).toBeUndefined();
      } finally {
        restoreEnv('NODE_ENV', original);
      }
    });

    it('should include stack traces outside production', () => {
      const originalNodeEnv = process.env.NODE_ENV;
      const originalKService = process.env.K_SERVICE;
      process.env.NODE_ENV = 'local';
      delete process.env.K_SERVICE;
      try {
        const error = new BadRequestException('test');
        expect(error.toJSON().stack).toBeDefined();
      } finally {
        restoreEnv('NODE_ENV', originalNodeEnv);
        restoreEnv('K_SERVICE', originalKService);
      }
    });

    it('should correctly identify http exceptions', () => {
      const httpError = new BadRequestException();
      const regularError = new Error();

      expect(isHttpException(httpError)).toBe(true);
      expect(isHttpException(regularError)).toBe(false);
    });

    it('should handle custom error objects', () => {
      const error = new BadRequestException({ foo: 'bar' });
      const json = error.toJSON();
      expect(error.getResponse()).toEqual({ foo: 'bar' });
      // message is inferred from constructor name or object message property
      expect(json.message).toBe('Bad Request Exception');
    });
  });

  describe('Factory', () => {
    it('should create correct exception types based on status', () => {
      const notFound = createHttpException(404);
      expect(notFound.constructor.name).toBe('NotFoundException');
      expect(notFound.getStatus()).toBe(404);

      const gateway = createHttpException(504);
      expect(gateway.constructor.name).toBe('GatewayTimeoutException');

      const teapot = createHttpException(418);
      expect(teapot.constructor.name).toBe('ImATeapotException');
    });

    it('should fallback to generic HttpException for unknown status', () => {
      const unknown = createHttpException(599, 'Unknown error');
      expect(unknown.constructor.name).toBe('HttpException');
      expect(unknown.getStatus()).toBe(599);
    });
  });

  describe('ValidationException', () => {
    it('should format validation errors correctly', () => {
      // Mock ValidationError from class-validator
      const errors = [
        {
          property: 'email',
          constraints: { isEmail: 'email must be an email' },
        },
        {
          property: 'nested',
          children: [
            {
              property: 'prop',
              constraints: { isString: 'prop must be a string' },
            },
          ],
        },
        // biome-ignore lint/suspicious/noExplicitAny: Mocking validation error
      ] as any;

      const { ValidationException } = require('../../src/error/validation.exception');
      const exception = new ValidationException(errors);
      const body = exception.getResponse();

      expect(exception.getStatus()).toBe(400);
      expect(body.message).toBe('Validation Failed');
      expect(body.errors).toHaveLength(2);
      expect(body.errors[0]).toEqual({ field: 'email', constraints: ['email must be an email'] });
      expect(body.errors[1]).toEqual({ field: 'nested.prop', constraints: ['prop must be a string'] });
    });
  });

  describe('AggregateException', () => {
    it('should aggregate multiple errors', () => {
      const errors = [new Error('Error 1'), new BadRequestException('Error 2')];
      const { AggregateException } = require('../../src/error/aggregate.exception');

      const exception = new AggregateException(errors);
      const json = exception.toJSON();

      expect(exception.getStatus()).toBe(500);
      expect(json.errors).toHaveLength(2);
      expect(json.errors[0].message).toBe('Error 1');
      expect((json.errors[1] as any).statusCode).toBe(400);
    });
  });

  describe('HttpException.from', () => {
    it('should return existing HttpException', () => {
      const existing = new BadRequestException();
      const result = HttpException.from(existing);
      expect(result).toBe(existing);
    });

    it('should wrap generic Error with a generic public message and keep the cause', () => {
      const err = new Error('Something went wrong');
      const result = HttpException.from(err);

      expect(result.getStatus()).toBe(500);
      // The internal message must not leak into the public response body; it is
      // preserved on `cause` for logs instead.
      expect(result.message).toBe('Internal Server Error');
      expect(result.cause).toBe(err);
      expect((result.cause as Error).message).toBe('Something went wrong');
    });

    it('should handle unknown objects', () => {
      const result = HttpException.from('unknown string');
      expect(result.getStatus()).toBe(500);
      expect(result.message).toBe('Unknown Error');
    });
  });

  describe('HttpStatus Enum', () => {
    it('should get correct status text', () => {
      expect(getHttpStatusText(200)).toBe('Ok');
      expect(getHttpStatusText(404)).toBe('Not Found');
      expect(getHttpStatusText(500)).toBe('Internal Server Error');
    });

    it('should get correct status code', () => {
      expect(getHttpStatusCode(200)).toBe('OK');
      expect(getHttpStatusCode(404)).toBe('NOT_FOUND');
    });
  });

  describe('error.utils', () => {
    describe('getDescriptionFrom', () => {
      it('should extract description from string', () => {
        expect(getDescriptionFrom('my error')).toBe('my error');
      });

      it('should extract description from options object', () => {
        expect(getDescriptionFrom({ description: 'from options' })).toBe('from options');
      });

      it('should return empty string when no description in options', () => {
        expect(getDescriptionFrom({})).toBe('');
      });
    });

    describe('getHttpExceptionOptionsFrom', () => {
      it('should return empty object for string input', () => {
        expect(getHttpExceptionOptionsFrom('description')).toEqual({});
      });

      it('should return the options object for object input', () => {
        const options = { description: 'test', cause: new Error('root') };
        const result = getHttpExceptionOptionsFrom(options);
        expect(result.description).toBe('test');
        expect(result.cause).toBeInstanceOf(Error);
      });
    });

    describe('extractDescriptionAndOptionsFrom', () => {
      it('should parse string input', () => {
        const result = extractDescriptionAndOptionsFrom('my desc');
        expect(result.description).toBe('my desc');
        expect(result.httpExceptionOptions).toEqual({});
      });

      it('should parse options input', () => {
        const result = extractDescriptionAndOptionsFrom({ description: 'opt desc' });
        expect(result.description).toBe('opt desc');
        expect(result.httpExceptionOptions.description).toBe('opt desc');
      });
    });

    describe('createBody', () => {
      it('should return statusCode and description when no object given', () => {
        const body = createBody(undefined, 'Not Found', 404);
        expect(body).toEqual({ statusCode: 404, message: 'Not Found' });
      });

      it('should return the object as-is for object input', () => {
        const body = createBody({ custom: 'data' }, 'desc', 400);
        expect(body).toEqual({ custom: 'data' });
      });

      it('should wrap string message with statusCode and error', () => {
        const body = createBody('Bad input', 'Bad Request', 400);
        expect(body).toEqual({ statusCode: 400, message: 'Bad input', error: 'Bad Request' });
      });
    });
  });
});
