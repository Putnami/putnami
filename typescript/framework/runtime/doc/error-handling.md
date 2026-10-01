# Error Handling

Complete guide to HTTP exception handling in `@putnami/runtime`.

## Overview

The error handling system provides:

- Typed HTTP exception classes for all standard status codes
- Automatic error serialization with `toJSON()`
- Validation error formatting
- Aggregate exception support
- Error factory for dynamic exception creation
- Type guards for error checking

## Standard HTTP Exceptions

### Client Errors (4xx)

#### BadRequestException (400)

```typescript
import { BadRequestException } from '@putnami/runtime';

// Simple message
throw new BadRequestException('Invalid email format');

// With error object
throw new BadRequestException({ reason: 'email_invalid', field: 'email' });

// With options
throw new BadRequestException('Invalid input', { cause: originalError });
```

#### UnauthorizedException (401)

```typescript
import { UnauthorizedException } from '@putnami/runtime';

throw new UnauthorizedException('Authentication required');
throw new UnauthorizedException({ message: 'Invalid token', code: 'INVALID_TOKEN' });
```

#### ForbiddenException (403)

```typescript
import { ForbiddenException } from '@putnami/runtime';

throw new ForbiddenException('Insufficient permissions');
throw new ForbiddenException({ message: 'Access denied', requiredRole: 'admin' });
```

#### NotFoundException (404)

```typescript
import { NotFoundException } from '@putnami/runtime';

throw new NotFoundException('User not found');
throw new NotFoundException({ message: 'Resource missing', resource: 'user', id: '123' });
```

#### ConflictException (409)

```typescript
import { ConflictException } from '@putnami/runtime';

throw new ConflictException('Email already exists');
throw new ConflictException({ message: 'Duplicate entry', field: 'email' });
```

#### UnprocessableEntityException (422)

```typescript
import { UnprocessableEntityException } from '@putnami/runtime';

throw new UnprocessableEntityException('Validation failed');
```

### Server Errors (5xx)

#### InternalServerErrorException (500)

```typescript
import { InternalServerErrorException } from '@putnami/runtime';

throw new InternalServerErrorException('An unexpected error occurred');
throw new InternalServerErrorException('Database error', { cause: dbError });
```

#### BadGatewayException (502)

```typescript
import { BadGatewayException } from '@putnami/runtime';

throw new BadGatewayException('Upstream service unavailable');
```

#### ServiceUnavailableException (503)

```typescript
import { ServiceUnavailableException } from '@putnami/runtime';

throw new ServiceUnavailableException('Service temporarily unavailable');
```

### Other Common Exceptions

- **MethodNotAllowedException** (405) - HTTP method not allowed
- **NotAcceptableException** (406) - Content negotiation failed
- **RequestTimeoutException** (408) - Request timeout
- **PayloadTooLargeException** (413) - Request body too large
- **TooManyRequestsException** (429) - Rate limit exceeded
- **GatewayTimeoutException** (504) - Gateway timeout

## Exception Options

All exceptions accept an optional options object:

```typescript
interface HttpExceptionOptions {
  cause?: Error;        // Original error that caused this exception
  description?: string; // Additional error description
}

throw new NotFoundException('User not found', {
  cause: originalError,
  description: 'The user was deleted or never existed'
});
```

## Validation Exceptions

Use `ValidationException` to format `class-validator` errors:

```typescript
import { ValidationException } from '@putnami/runtime';
import { validate } from 'class-validator';

class CreateUserDto {
  @IsString() @IsEmail() email: string;
  @IsString() @MinLength(8) password: string;
}

const dto = new CreateUserDto();
dto.email = 'invalid-email';
dto.password = 'short';

const errors = await validate(dto);
if (errors.length > 0) {
  throw new ValidationException(errors);
  // Returns 400 with formatted errors:
  // {
  //   statusCode: 400,
  //   message: "Validation Failed",
  //   errors: [
  //     { field: "email", constraints: ["isEmail"] },
  //     { field: "password", constraints: ["minLength"] }
  //   ]
  // }
}
```

### Validation Error Format

Validation errors are automatically formatted into a flat structure:

```typescript
interface ValidationErrorResponse {
  field: string;        // Field name (supports nested: "user.email")
  constraints: string[]; // Array of constraint names
}
```

## Aggregate Exceptions

Handle multiple errors in batch operations:

```typescript
import { AggregateException } from '@putnami/runtime';

const errors: Error[] = [];

// Collect errors from parallel operations
try {
  await processUser(user1);
} catch (error) {
  errors.push(error);
}

try {
  await processUser(user2);
} catch (error) {
  errors.push(error);
}

if (errors.length > 0) {
  throw new AggregateException(errors, 'Failed to process some users');
  // Returns 500 with all errors:
  // {
  //   statusCode: 500,
  //   message: "Failed to process some users",
  //   errors: [
  //     { message: "...", name: "...", code: "..." },
  //     { message: "...", name: "...", code: "..." }
  //   ]
  // }
}
```

## Error Factory

Create exceptions dynamically based on status codes:

```typescript
import { createHttpException, HttpStatus } from '@putnami/runtime';

const status = 404;
const error = createHttpException(status, 'Resource not found');
// error is instance of NotFoundException

// With options
const error2 = createHttpException(
  HttpStatus.BAD_REQUEST,
  'Invalid input',
  { cause: originalError }
);
```

## Error Serialization

All exceptions have a `toJSON()` method for serialization:

```typescript
import { NotFoundException } from '@putnami/runtime';

const error = new NotFoundException('User not found');
const json = error.toJSON();
// {
//   statusCode: 404,
//   message: "User not found",
//   error: "Not Found"
// }
```

## Stack Disclosure

`toJSON()` adds a `stack` field only outside production. The decision comes from
`shouldExposeErrorStack()`, which reports `false` when `NODE_ENV` is
`production` or `prod`, or when `K_SERVICE` is set (Cloud Run). Use it directly
when you render your own error surface, so the framework and your code agree:

```typescript
import { shouldExposeErrorStack } from '@putnami/runtime';

if (shouldExposeErrorStack()) {
  renderStack(error.stack);
}
```

Call it — do not re-implement it as `process.env.NODE_ENV !== 'production'`.
Bundlers replace that exact expression with its build-time value, so a package
that ships the inline form freezes the decision at publication and your own
`NODE_ENV` never reaches it. `shouldExposeErrorStack()` reads the environment
through an indirection the bundler cannot fold, so the decision stays yours at
run time.

A runtime with no `process.env` — a browser bundle — reports `false`: with no
environment to read it cannot assert that it is development, and a guard that
cannot tell must assume production. This is narrower than "`NODE_ENV` is unset"
— a server process without `NODE_ENV` still exposes, because running `bun run`
without setting it is a normal development shape.

**Do not call this from client code.** A bundler that shims `process.env` for
the browser fabricates an empty environment that reads exactly like a
developer's machine. `@putnami/web` exports `shouldExposeClientErrors()` for
client error surfaces: in a browser it consults only `window.__putnamiExposeErrors`,
a flag SSR emits when the server itself exposes, so the server-rendered and the
hydrated markup agree in both environments.

## Type Guards

Check if an error is an HTTP exception:

```typescript
import { isHttpException, HttpException } from '@putnami/runtime';

try {
  // ...
} catch (error) {
  if (isHttpException(error)) {
    console.log(error.statusCode); // TypeScript knows this is HttpException
    console.log(error.toJSON());
  } else {
    // Handle non-HTTP errors
  }
}
```

## Wrapping Unknown Errors

Safely convert any error into an `HttpException`:

```typescript
import { HttpException } from '@putnami/runtime';

try {
  // Some operation that might throw
  await riskyOperation();
} catch (error) {
  // If it's already an HttpException, return it as-is
  // Otherwise, wrap it in InternalServerErrorException
  throw HttpException.from(error);
}
```

## Common Patterns

### Route Handler Error Handling

```typescript
import { NotFoundException, BadRequestException } from '@putnami/runtime';

export async function getUser(id: string) {
  if (!id) {
    throw new BadRequestException('User ID is required');
  }

  const user = await db.users.findById(id);
  if (!user) {
    throw new NotFoundException(`User with ID ${id} not found`);
  }

  return user;
}
```

### Validation in Services

```typescript
import { ValidationException } from '@putnami/runtime';
import { validate } from 'class-validator';

@Service()
class UserService {
  async createUser(data: CreateUserDto) {
    const errors = await validate(data);
    if (errors.length > 0) {
      throw new ValidationException(errors);
    }

    return await this.userRepo.create(data);
  }
}
```

### Error Wrapping

```typescript
import { HttpException, InternalServerErrorException } from '@putnami/runtime';

@Service()
class DatabaseService {
  async query(sql: string) {
    try {
      return await this.db.query(sql);
    } catch (error) {
      // Wrap database errors
      throw HttpException.from(error);
    }
  }
}
```

### Batch Processing

```typescript
import { AggregateException } from '@putnami/runtime';

async function processBatch(items: Item[]) {
  const errors: Error[] = [];

  for (const item of items) {
    try {
      await processItem(item);
    } catch (error) {
      errors.push(error as Error);
    }
  }

  if (errors.length > 0) {
    throw new AggregateException(errors, 'Some items failed to process');
  }
}
```

## HTTP Status Codes

Use the `HttpStatus` enum for type-safe status codes:

```typescript
import { HttpStatus, createHttpException } from '@putnami/runtime';

const error = createHttpException(HttpStatus.NOT_FOUND, 'Not found');
```

Available status codes include all standard HTTP status codes (200-599).

## Best Practices

1. **Use specific exceptions**: Prefer `NotFoundException` over generic `HttpException`
2. **Include context**: Provide meaningful error messages
3. **Preserve original errors**: Use the `cause` option to chain errors
4. **Validate early**: Use `ValidationException` for input validation
5. **Handle batch errors**: Use `AggregateException` for parallel operations
6. **Type safety**: Use type guards to check error types

## Error Response Format

All exceptions serialize to a consistent format:

```typescript
{
  statusCode: number;  // HTTP status code
  message: string | object; // Error message or object
  error?: string;      // Error name (e.g., "Not Found")
}
```

Custom exceptions can override `toJSON()` to provide additional fields.

## Troubleshooting

### Error Not Serializing

Make sure you're calling `toJSON()` on the exception, or that your error handler properly serializes `HttpException` instances.

### Validation Errors Not Formatted

Use `ValidationException` instead of `BadRequestException` when you have `class-validator` errors.

### Original Error Lost

Use the `cause` option to preserve the original error:

```typescript
throw new InternalServerErrorException('Database error', { cause: dbError });
```

## Next Steps

- Learn about [Logging](logging.md) for error logging
- Explore [Context Management](context-management.md) for context-aware error handling
- Check the [API Reference](api-reference.md) for complete exception class signatures
