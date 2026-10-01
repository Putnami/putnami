import { application, api, http, logger, oAuth2, platform, redirect } from '@putnami/application';

function requireEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`Missing required environment variable: ${name}`);
  return value;
}

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/profile')))
    .use(logger())
    .use(platform())
    .use(
      oAuth2({
        authorizeUri: 'https://accounts.google.com/o/oauth2/v2/auth',
        tokenUri: 'https://oauth2.googleapis.com/token',
        clientId: requireEnv('OAUTH_CLIENT_ID'),
        clientSecret: requireEnv('OAUTH_CLIENT_SECRET'),
        scopes: ['openid', 'email', 'profile'],
      }),
    )
    .use(api());
