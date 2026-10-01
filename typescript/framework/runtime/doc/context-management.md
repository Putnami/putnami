# Context Management

Complete guide to context management using async local storage in `@putnami/runtime`.

## Overview

The context management system provides:

- Async-local storage for request-scoped data
- Type-safe context access
- Automatic propagation through async call stacks
- Integration with dependency injection for request-scoped services

## Basic Usage

### Running Code in Context

Use `runInContext` to execute code within a context:

```typescript
import { runInContext, useContext } from '@putnami/runtime';

interface RequestContext {
  userId: string;
  requestId: string;
}

await runInContext({ userId: '123', requestId: 'abc' }, async () => {
  // Access context anywhere in this call stack
  const ctx = useContext<RequestContext>();
  console.log(ctx.userId); // '123'
  console.log(ctx.requestId); // 'abc'
});
```

### Accessing Context

Use `useContext()` to access the current context from anywhere in the call stack:

```typescript
import { useContext } from '@putnami/runtime';

function someDeepFunction() {
  const ctx = useContext<RequestContext>();
  // ctx is available without passing it as a parameter
  return ctx.userId;
}

await runInContext({ userId: '123' }, async () => {
  const userId = someDeepFunction(); // '123'
});
```

## Integration with Dependency Injection

Context works seamlessly with scoped services via `ctx.scope()`:

```typescript
import { ContainerContext, provide } from '@putnami/runtime';

class RequestData {
  requestId = crypto.randomUUID();
  startTime = Date.now();
}

class UserService {
  constructor(private req: RequestData) {}
  getRequestId() {
    return this.req.requestId;
  }
}

const ctx = new ContainerContext('app');
ctx.register(provide(RequestData, { scope: 'scoped' }));
ctx.register(provide(UserService, { deps: [RequestData] }));

await ctx.start();

// Each scope gets fresh scoped instances
await ctx.scope(async (scope) => {
  const service = scope.get(UserService);
  console.log(service.getRequestId()); // Fresh per scope
});

await ctx.close();
```

## Common Patterns

### Request ID Tracking

```typescript
class RequestData {
  requestId = crypto.randomUUID();
  startTime = Date.now();
}

class RequestLogger {
  constructor(private req: RequestData) {}

  log(message: string) {
    const duration = Date.now() - this.req.startTime;
    console.log(`[${this.req.requestId}] ${message} (${duration}ms)`);
  }
}

const app = application()
  .provide(RequestData, { scope: 'scoped' })
  .provide(RequestLogger, { deps: [RequestData] });

await app.start();

await app.scope(async (scope) => {
  const logger = scope.get(RequestLogger);
  logger.log('Processing request'); // [abc-123] Processing request (5ms)
});
```

### Nested Contexts

Contexts can be nested, with inner contexts inheriting from outer ones:

```typescript
await runInContext({ userId: '123' }, async () => {
  const outerCtx = useContext();
  console.log(outerCtx.userId); // '123'

  await runInContext({ requestId: 'abc' }, async () => {
    const innerCtx = useContext();
    console.log(innerCtx.userId); // '123' (inherited)
    console.log(innerCtx.requestId); // 'abc' (new)
  });
});
```

## Type Safety

Define interfaces for type-safe context access:

```typescript
interface MyRequestContext {
  userId: string;
  requestId: string;
  metadata: {
    ip: string;
    userAgent: string;
  };
}

// Type-safe access
const ctx = useContext<MyRequestContext>();
console.log(ctx.userId); // TypeScript knows this exists
```

## Best Practices

1. **Define context interfaces**: Use TypeScript interfaces for type safety
2. **Initialize in middleware**: Set context at the start of request handling
3. **Use request-scoped services**: Combine with DI for clean architecture
4. **Keep context minimal**: Only store request-specific data
5. **Don't mutate shared state**: Context should be request-specific

## Integration with HTTP Handlers

In a typical HTTP application, `app.scope()` wraps each request:

```typescript
// The framework handles this automatically, but conceptually:
export async function handler(req: Request) {
  return await app.scope(async (scope) => {
    const userService = scope.get(UserService);
    return userService.getCurrentUser();
  });
}
```

## Troubleshooting

### Context is Undefined

If `useContext()` returns `undefined`, make sure you're calling it inside a `runInContext` block.

### Context Not Propagating

Context only propagates through async operations. Synchronous code outside the async chain won't have access.

### Scoped Services Not Working

Scoped services require a scope. Make sure you're calling `scope.get()` inside `app.scope()`.

## Next Steps

- Learn about [Dependency Injection](dependency-injection.md) for request-scoped services
- Explore [Error Handling](error-handling.md) for context-aware error handling
- Check the [API Reference](api-reference.md) for complete function signatures
