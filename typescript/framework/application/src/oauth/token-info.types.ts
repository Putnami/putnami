/**
 * Token endpoint response (RFC 6749 §5.1 + OIDC Core §3.1.3.3).
 */
export interface TokenInfo {
  access_token: string;
  refresh_token?: string;
  expires_in: number;
  scope?: string;
  token_type: string;
  /** OIDC id_token, present when the `openid` scope was requested. */
  id_token?: string;
}
