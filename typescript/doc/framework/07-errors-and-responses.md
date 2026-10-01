# Errors & Responses

Putnami provides typed HTTP exceptions and response factories for building consistent APIs with proper error handling.

## HTTP exceptions

### Built-in exceptions

Import and throw HTTP exceptions from `@putnami/runtime`:

```ts
import {
  BadRequestException,      // 400
  UnauthorizedException,    // 401
  ForbiddenException,       // 403
  NotFoundException,        // 404
  MethodNotAllowedException, // 405
  ConflictException,        // 409
  PayloadTooLargeException, // 413
  UnprocessableEntityException, // 422
  TooManyRequestsException, // 429
  InternalServerErrorException, // 500
  NotImplementedException,  // 501
  ServiceUnavailableException, // 503
} from '@putnami/runtime';
```

### Throwing exceptions

```ts
import { endpoint, Uuid } from '@putnami/application';
import { NotFoundException, ForbiddenException } from '@putnami/runtime';

export default endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const user = await findUser(ctx.params.id);

    if (!user) {
      throw new NotFoundException(`User ${ctx.params.id} not found`);
    }

    if (!canAccess(ctx, user)) {
      throw new ForbiddenException('Access denied');
    }

    return user;
  });
```

### Exception with details

```ts
throw new BadRequestException('Validation failed', {
  fields: {
    email: 'Invalid email format',
    password: 'Password too short',
  },
});
```

### Custom exceptions

Create custom exceptions by extending `HttpException`:

```ts
import { HttpException } from '@putnami/runtime';

export class RateLimitException extends HttpException {
  constructor(retryAfter: number) {
    super(429, `Rate limit exceeded. Retry after ${retryAfter} seconds`);
    this.retryAfter = retryAfter;
  }

  retryAfter: number;
}

// Usage
throw new RateLimitException(60);
```

## Validation

The `endpoint()` builder validates inputs automatically. Invalid requests are rejected before your handler runs:

```ts
import { endpoint, Email, MinLength } from '@putnami/application';

export default endpoint()
  .body({ email: Email, password: MinLength(8) })
  .handle(async (ctx) => {
    const body = await ctx.body();
    // body.email and body.password are validated
  });
```

## Context throw

Use `ctx.throw()` for quick error responses:

```ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  const id = ctx.queryParams().get('id');

  if (!id) {
    ctx.throw(400, 'ID is required');
  }

  return { id };
});
```

## Response factories

### Creating responses

```ts
import { endpoint, HttpResponse, json } from '@putnami/application';

// JSON response
export default endpoint(() => {
  return json({ message: 'Hello' });
});

// JSON with status
export default endpoint(() => {
  return json({ created: true }, { status: 201 });
});

// Redirect
export default endpoint(() => {
  return HttpResponse.redirect('/new-location');
});

// Redirect with status
export default endpoint(() => {
  return HttpResponse.redirect('/permanent', 301);
});
```

### Error response factories

```ts
import { badRequest, unauthorized, forbidden, notFound, internalServerError } from '@putnami/application';

// 400 Bad Request
badRequest('Invalid input');

// 401 Unauthorized
unauthorized('Authentication required');

// 403 Forbidden
forbidden('Access denied');

// 404 Not Found
notFound('Resource not found');

// 500 Internal Server Error
internalServerError('Something went wrong');
```

### Custom headers

```ts
import { endpoint, json } from '@putnami/application';

export default endpoint(() => {
  return json({ data: 'value' })
    .setHeader('X-Request-Id', crypto.randomUUID())
    .setHeader('Cache-Control', 'max-age=3600');
});
```

### Multiple headers with same name

```ts
import { endpoint, json } from '@putnami/application';

export default endpoint(() => {
  return json({ ok: true })
    .pushHeader('Set-Cookie', 'session=abc; HttpOnly')
    .pushHeader('Set-Cookie', 'tracking=xyz');
});
```

### Custom response

```ts
import { endpoint, HttpResponse } from '@putnami/application';

export default endpoint(() => {
  return new HttpResponse(new Response('Plain text', {
    status: 200,
    headers: {
      'Content-Type': 'text/plain',
    },
  }));
});
```

### File download

```ts
import { endpoint, HttpResponse } from '@putnami/application';

export default endpoint(async () => {
  const file = Bun.file('report.pdf');
  const content = await file.arrayBuffer();

  return new HttpResponse(new Response(content, {
    headers: {
      'Content-Type': 'application/pdf',
      'Content-Disposition': 'attachment; filename="report.pdf"',
      'Content-Length': String(content.byteLength),
    },
  }));
});
```

### Streaming response

```ts
import { endpoint, HttpResponse } from '@putnami/application';

export default endpoint(() => {
  const stream = new ReadableStream({
    async start(controller) {
      for (let i = 0; i < 10; i++) {
        controller.enqueue(`data: ${i}\n\n`);
        await new Promise(r => setTimeout(r, 1000));
      }
      controller.close();
    },
  });

  return new HttpResponse(new Response(stream, {
    headers: {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
    },
  }));
});
```

## Error handling patterns

### Global error handler

```ts
import type { HttpMiddleware } from '@putnami/application';
import { HttpException, ValidationException } from '@putnami/runtime';
import { json, internalServerError } from '@putnami/application';

export const errorHandler: HttpMiddleware = async (ctx, next) => {
  try {
    return await next();
  } catch (error) {
    // Known HTTP exceptions
    if (error instanceof HttpException) {
      return json(
        { error: error.message, code: error.status },
        { status: error.status }
      );
    }

    // Validation errors
    if (error instanceof ValidationException) {
      return json(
        { error: 'Validation failed', details: error.errors },
        { status: 422 }
      );
    }

    // Log unexpected errors
    console.error('Unexpected error:', error);

    return internalServerError('An unexpected error occurred');
  }
};
```

### Domain-specific errors

```ts
import { endpoint, json } from '@putnami/application';

// Define domain errors
export class InsufficientFundsError extends Error {
  constructor(public available: number, public required: number) {
    super(`Insufficient funds: ${available} available, ${required} required`);
  }
}

// Handle in route
export default endpoint(async () => {
  try {
    await processPayment(amount);
    return { success: true };
  } catch (error) {
    if (error instanceof InsufficientFundsError) {
      return json(
        {
          error: 'Insufficient funds',
          available: error.available,
          required: error.required,
        },
        { status: 402 }
      );
    }
    throw error;
  }
});
```

### Error aggregation

```ts
import { endpoint } from '@putnami/application';
import { AggregateException } from '@putnami/runtime';

export default endpoint(async (ctx) => {
  const body = await ctx.body();
  const errors: Error[] = [];

  // Collect multiple errors
  if (!isValidEmail(body.email)) {
    errors.push(new Error('Invalid email'));
  }

  if (!isValidPassword(body.password)) {
    errors.push(new Error('Invalid password'));
  }

  if (errors.length > 0) {
    throw new AggregateException(errors);
  }

  // Process valid input...
});
```

## Response status codes

### Success codes

```ts
import { endpoint, HttpResponse, json } from '@putnami/application';

// 200 OK (default)
return { data: 'value' };

// 201 Created
return json({ id: newId }, { status: 201 });

// 204 No Content
export default endpoint(async () => {
  await deleteResource();
  return undefined;
});
```

### Redirect codes

```ts
import { HttpResponse } from '@putnami/application';

// 301 Moved Permanently
HttpResponse.redirect('/new-url', 301);

// 302 Found (default)
HttpResponse.redirect('/new-url');

// 303 See Other
HttpResponse.redirect('/new-url', 303);

// 307 Temporary Redirect
HttpResponse.redirect('/new-url', 307);
```

### Client error codes

```ts
// 400 Bad Request
throw new BadRequestException('Invalid input');

// 401 Unauthorized
throw new UnauthorizedException('Please log in');

// 403 Forbidden
throw new ForbiddenException('Not allowed');

// 404 Not Found
throw new NotFoundException('Resource not found');

// 409 Conflict
throw new ConflictException('Resource already exists');

// 422 Unprocessable Entity
throw new ValidationException(errors);

// 429 Too Many Requests
throw new TooManyRequestsException('Rate limit exceeded');
```

### Server error codes

```ts
// 500 Internal Server Error
throw new InternalServerErrorException('Server error');

// 501 Not Implemented
throw new NotImplementedException('Feature not available');

// 503 Service Unavailable
throw new ServiceUnavailableException('Service temporarily unavailable');
```

## Related guides

- [API](/docs/frameworks/typescript/api)
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware)
