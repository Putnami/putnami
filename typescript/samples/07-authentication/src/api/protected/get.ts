import { endpoint } from '@putnami/application';

// GET /protected — Example of a protected resource
// .secure() ensures the request has a valid identity before reaching the handler
export const GET = endpoint()
  .secure()
  .handle((ctx) => {
    const user = ctx['user'] as { sub?: string; email?: string };

    return {
      message: 'You have access to this protected resource',
      user: { id: user.sub, email: user.email },
      accessedAt: new Date().toISOString(),
    };
  });
