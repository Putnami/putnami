export * from './oauth.config';
export * from './oauth.middleware';
export * from './oauth.plugin';
// Re-export the public OAuth service API explicitly. `setActiveOAuthService` is
// @internal — it mutates the process-global auth singleton and is set only by
// OAuthPlugin during warmup/stop — so it is deliberately kept off the public
// surface; exposing it would let a consumer disable authentication globally.
export type { AuthorizeUrlOptions, ExchangeCodeOptions, ResolvedEndpoints, VerifyOptions } from './oauth.service';
export {
  computeExpireAt,
  generateCodeChallenge,
  generateCodeVerifier,
  OAuthService,
  useOAuthService,
} from './oauth.service';
export * from './oauth.utils';
