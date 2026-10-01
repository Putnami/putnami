import { tryContext } from '@putnami/runtime';
import type { ClientRequest, ClientResponse, Interceptor } from '../runtime/transport.type';

/**
 * Auth context available in the async context (set by incoming request middleware).
 */
interface AuthContext {
  /** JWT token from the incoming request, forwarded to downstream services */
  user?: Record<string, unknown>;
  /** Raw authorization header from the incoming request, captured by buildHttpContext */
  __authorizationHeader?: string;
}

/** Header used to declare client identity on outgoing requests. */
export const CLIENT_ID_HEADER = 'X-Client-Id';

/** Pattern for valid client IDs — alphanumeric, hyphens, underscores, dots. */
const CLIENT_ID_RE = /^[a-zA-Z0-9._-]+$/;

/**
 * Auth interceptor options.
 */
export interface AuthInterceptorOptions {
  /**
   * Client identity — sent as `X-Client-Id` header on every request.
   * The receiving service can verify this to control which services can call it.
   */
  clientId?: string;
  /**
   * Async function that returns a bearer token for M2M authentication.
   * Used when no incoming user JWT is available (e.g. background jobs, cron tasks).
   *
   * Typically wired to `OAuthService.clientToken()`:
   * ```typescript
   * authInterceptor({ tokenProvider: () => get(OAuthService).clientToken() })
   * ```
   */
  tokenProvider?: () => Promise<string | undefined>;
}

/**
 * Creates an interceptor that handles authentication for outgoing client calls.
 *
 * Three-layer auth strategy (first match wins):
 * 1. **Existing header** — if `Authorization` is already set, don't touch it
 * 2. **JWT forwarding** — propagate the user's JWT from the incoming request context
 * 3. **Client credentials** — obtain a M2M token via the `tokenProvider` callback
 *
 * Additionally injects `X-Client-Id` to declare the calling service's identity.
 * The receiving service can use `requireClient()` to restrict access.
 */
export function authInterceptor(options?: AuthInterceptorOptions): Interceptor;
export function authInterceptor(tokenProvider?: () => Promise<string | undefined>): Interceptor;
export function authInterceptor(
  optionsOrProvider?: AuthInterceptorOptions | (() => Promise<string | undefined>),
): Interceptor {
  const options: AuthInterceptorOptions =
    typeof optionsOrProvider === 'function' ? { tokenProvider: optionsOrProvider } : (optionsOrProvider ?? {});

  // Validate clientId at construction time to fail fast on misconfiguration
  if (options.clientId && !CLIENT_ID_RE.test(options.clientId)) {
    throw new Error(`Invalid clientId "${options.clientId}": must match [a-zA-Z0-9._-]+`);
  }

  return async (request: ClientRequest, next: (req: ClientRequest) => Promise<ClientResponse>) => {
    // Inject client identity
    if (options.clientId && !request.headers.has(CLIENT_ID_HEADER)) {
      request.headers.set(CLIENT_ID_HEADER, options.clientId);
    }

    // Don't override Authorization if already set
    if (request.headers.has('Authorization')) {
      return next(request);
    }

    // Layer 1: Forward user JWT from incoming request context
    const context = tryContext<AuthContext>();
    if (context?.__authorizationHeader) {
      request.headers.set('Authorization', context.__authorizationHeader);
      return next(request);
    }

    // Layer 2: Client credentials token (M2M)
    if (options.tokenProvider) {
      const token = await options.tokenProvider();
      if (token) {
        request.headers.set('Authorization', `Bearer ${token}`);
      }
    }

    return next(request);
  };
}
