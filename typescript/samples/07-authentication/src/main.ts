import { application, api, http, logger, oAuth2, platform, redirect } from '@putnami/application';
import { requireOAuthCredentials } from './oauth-credentials';

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/profile')))
    .use(logger())
    .use(platform())
    .use(requireOAuthCredentials())
    .use(
      oAuth2({
        authorizeUri: 'https://accounts.google.com/o/oauth2/v2/auth',
        tokenUri: 'https://oauth2.googleapis.com/token',
        scopes: ['openid', 'email', 'profile'],
      }),
    )
    .use(api());
