import { type InferConfig, useConfig } from '@putnami/runtime';
import type { Module, Plugin } from '../application';
import { HttpPlugin } from '../http/http.plugin';
import { IdentityResolverMiddleware } from '../security/identity-resolver.middleware';
import { CallbackHandler } from './handlers/callback.get';
import { LoginHandler } from './handlers/login.get';
import { LogoutHandler } from './handlers/logout.get';
import { OAuthConfig } from './oauth.config';
import { OAuthService, setActiveOAuthService } from './oauth.service';

/**
 * OAuth2 / OpenID Connect authentication plugin.
 *
 * Registers `loginRoute`, `callbackRoute` (when distinct from `loginRoute`),
 * and `logoutRoute` against `HttpPlugin`. Owns a process-wide `OAuthService`
 * accessible via `useOAuthService()`.
 *
 * @example Putnami Auth (OIDC discovery)
 * ```ts
 * application()
 *   .use(http())
 *   .use(oAuth2({
 *     discoveryUri: 'https://auth.putnami.cloud/.well-known/openid-configuration',
 *     callbackRoute: '/auth/callback',
 *     redirectUri: 'https://app.example.com/auth/callback',
 *     scopes: ['openid', 'profile', 'email'],
 *   }));
 * ```
 *
 * @example Generic OAuth2 provider with manual endpoints
 * ```ts
 * application()
 *   .use(http())
 *   .use(oAuth2({
 *     authorizeUri: 'https://github.com/login/oauth/authorize',
 *     tokenUri: 'https://github.com/login/oauth/access_token',
 *     scopes: ['read:user'],
 *   }));
 * ```
 */
export class OAuthPlugin implements Plugin {
  readonly service = new OAuthService();

  constructor(private providedConfig?: Partial<InferConfig<typeof OAuthConfig>>) {}

  async warmup(app: Module): Promise<void> {
    this.service.confInit = this.providedConfig;
    setActiveOAuthService(this.service);

    const httpPlugin = await app.ensurePlugin(HttpPlugin);
    if (!httpPlugin) return;

    const config = useConfig(OAuthConfig, { confInit: this.providedConfig });

    httpPlugin.prepend(
      IdentityResolverMiddleware({
        issuer: config.issuer,
        audience: config.audience,
      }),
    );

    httpPlugin.route('GET', config.loginRoute, LoginHandler);
    httpPlugin.route('GET', config.logoutRoute, LogoutHandler);

    const callbackRoute = config.callbackRoute ?? config.loginRoute;
    if (callbackRoute !== config.loginRoute) {
      httpPlugin.route('GET', callbackRoute, CallbackHandler);
    }
  }

  async stop(): Promise<void> {
    setActiveOAuthService(undefined);
  }
}

/**
 * Factory function for creating an OAuth2 plugin.
 */
export const oAuth2 = (config?: Partial<InferConfig<typeof OAuthConfig>>): OAuthPlugin => new OAuthPlugin(config);
