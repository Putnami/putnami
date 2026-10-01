import { endpoint, accessToken, useOAuthService } from '@putnami/application';

// GET /auth/session — Inspect the current session (works for both authenticated and anonymous)
// accessToken({ redirect: false }) retrieves the token without redirecting unauthenticated users
export const GET = endpoint().handle(async () => {
  const token = await accessToken({ redirect: false });

  if (!token) {
    return {
      authenticated: false,
      message: 'Not logged in. Visit /login to authenticate.',
    };
  }

  const oauthService = useOAuthService();
  const claims = await oauthService.verify<{ sub: string; email: string; name: string }>(token);

  if (!claims) {
    return {
      authenticated: false,
      message: 'Invalid or expired token. Visit /login to re-authenticate.',
    };
  }

  return {
    authenticated: true,
    user: {
      id: claims.sub,
      email: claims.email,
      name: claims.name,
    },
  };
});
