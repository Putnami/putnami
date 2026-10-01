import { useOAuthService, type VerifyOptions } from '../../oauth/oauth.service';
import { PrincipalKind } from '../identity.constants';
import type { Principal } from '../security.types';
import { type AuthStrategy, extractBearerToken } from './auth-strategy';

/** Configures the {@link bearerJwtStrategy}. */
export interface BearerJwtStrategyOptions {
  /**
   * Expected token issuer(s). When set, a token whose `iss` claim does not match
   * is rejected. Mirrors `JWKSJWTConfig.RequiredIssuer`.
   */
  issuer?: string | string[];
  /**
   * Expected token audience(s). When set, a token whose `aud` claim does not
   * contain one of these is rejected (token-confusion defense). Mirrors
   * `JWKSJWTConfig.Audience`.
   */
  audience?: string | string[];
}

/**
 * Authenticates a request by a bearer JWT validated through the active
 * {@link OAuthService} — reusing its existing RS256/JWKS verifier and JWKS cache
 * (`oauth-jwt.ts` + `oauth.service.ts`). It deliberately adds no new crypto and
 * no second JWKS cache; it is the composable-strategy face of the OAuth resolver
 * the framework already ships. TypeScript twin of the JWKS path in
 * `go/framework/security/jwt.go` (`JWKSJWT`).
 *
 * It fails closed on every unauthenticated case — no bearer token, no active
 * OAuthService, or a token that fails signature/issuer/audience/expiry checks —
 * by resolving `undefined` rather than throwing.
 */
export const bearerJwtStrategy = (options: BearerJwtStrategyOptions = {}): AuthStrategy => {
  return async (ctx) => {
    const token = extractBearerToken(ctx);
    if (!token) {
      return undefined;
    }

    // No OAuthService registered => nothing can verify the token. Fail closed
    // (behave as "not authenticated") rather than propagate the setup error.
    let oauthService: ReturnType<typeof useOAuthService>;
    try {
      oauthService = useOAuthService();
    } catch {
      return undefined;
    }

    const verifyOptions: VerifyOptions = {};
    if (options.issuer) {
      verifyOptions.issuer = options.issuer;
    }
    if (options.audience) {
      verifyOptions.audience = options.audience;
    }

    // `verify` returns undefined on an invalid token, but can throw when the JWKS
    // endpoint is unconfigured; treat any failure as "not authenticated".
    try {
      const claims = await oauthService.verify<Record<string, unknown>>(token, verifyOptions);
      if (!claims) {
        return undefined;
      }
      // A verified bearer JWT is a `user` principal in the PrincipalKind
      // vocabulary. Stamp it as the default (a `kind` claim in the token, if
      // present, overrides) so `.secure({ principalKind: 'user' })` is
      // satisfiable for the framework's own bearer flow — mirroring
      // apiKeyStrategy, which stamps PrincipalKind.ApiKey.
      return { kind: PrincipalKind.User, ...claims } as Principal;
    } catch {
      return undefined;
    }
  };
};
