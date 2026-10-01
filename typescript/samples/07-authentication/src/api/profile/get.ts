import { endpoint } from '@putnami/application';

// GET /profile — Protected route requiring authentication
// .secure() verifies the user is authenticated; ctx['user'] is populated by the global identity resolver
export const GET = endpoint()
  .secure()
  .handle((ctx) => {
    const user = ctx['user'] as { sub?: string; email?: string; name?: string };

    return {
      profile: {
        id: user.sub,
        email: user.email,
        name: user.name,
      },
    };
  });
