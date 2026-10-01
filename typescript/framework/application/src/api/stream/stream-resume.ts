import type { WebSocketDiagnostic } from './websocket-protocol';

/**
 * Bounds this provider applies to every resume grant it issues. They are
 * framework ceilings, not defaults that enable anything: a stream is continued
 * only because its endpoint declared resume.
 */
export const RESUME_BOUNDS = Object.freeze({
  /**
   * How long a grant stays redeemable. A consumer that reconnects later starts
   * a fresh stream instead of being handed a position nobody can still prove is
   * gap-free.
   */
  ttlMs: 60_000,
  /**
   * How many times one stream may be continued. Without it a socket that keeps
   * breaking keeps one position alive forever.
   */
  budget: 5,
  /**
   * The live grants of one endpoint. The oldest is dropped first, so a flood of
   * abandoned streams cannot grow this map without limit.
   */
  grants: 256,
  /**
   * The entropy of one token, in bytes. The token is the only thing standing
   * between a stolen position and a replayed stream, so it is generated, never
   * derived from anything a peer can see.
   */
  tokenBytes: 32,
});

/**
 * One live permission to continue one stream.
 *
 * It is bound to the operation and the client identity that earned it, spent by
 * its single redemption, and replaced by a fresh grant on every ready frame.
 * That rotation is what makes an observed token useless after the connection
 * that carried it ends.
 */
export interface ResumeGrant {
  readonly operationId: string;
  readonly clientId: string;
  /**
   * The highest sequence this provider has put on the wire for the stream this
   * grant continues. A consumer cannot ask to continue past it.
   */
  cursor: bigint;
  /** The number of continuations still allowed on this stream. */
  readonly budget: number;
  readonly expiresAt: number;
}

/** What one redemption answered: the grant, or why it bought nothing. */
export type ResumeRedemption = { readonly grant: ResumeGrant } | { readonly refusal: WebSocketDiagnostic };

/**
 * The live resume grants of one stream endpoint.
 *
 * A grant lives in memory only: a provider that restarts cannot prove any
 * position is still gap-free, so it refuses the continuation and the consumer
 * learns the stream ended instead of silently receiving a second copy.
 */
export class ResumeGrantStore {
  private readonly grants = new Map<string, ResumeGrant>();
  private readonly now: () => number;
  private readonly mint: () => string;

  constructor(options?: { readonly now?: () => number; readonly mint?: () => string }) {
    this.now = options?.now ?? Date.now;
    this.mint = options?.mint ?? mintResumeToken;
  }

  /** The number of grants currently held. Bounded by `RESUME_BOUNDS.grants`. */
  get size(): number {
    return this.grants.size;
  }

  /**
   * Mint the grant one ready frame carries. `budget` is the number of
   * continuations the new grant still allows, so a continued stream inherits
   * what the grant it spent had left rather than starting over.
   */
  issue(
    operationId: string,
    clientId: string,
    cursor: bigint,
    budget: number,
  ): { token: string; grant: ResumeGrant } | undefined {
    if (budget <= 0) return undefined;
    this.evictExpired();
    while (this.grants.size >= RESUME_BOUNDS.grants) {
      const oldest = this.grants.keys().next();
      if (oldest.done) break;
      this.grants.delete(oldest.value);
    }
    const token = this.mint();
    const grant: ResumeGrant = {
      operationId,
      clientId,
      cursor,
      budget,
      expiresAt: this.now() + RESUME_BOUNDS.ttlMs,
    };
    this.grants.set(token, grant);
    return { token, grant };
  }

  /**
   * Spend one grant. A grant is single use: the token that opened one
   * connection can never open another, whoever holds it.
   */
  redeem(token: string, operationId: string, clientId: string, afterSequence: bigint): ResumeRedemption {
    this.evictExpired();
    const grant = this.grants.get(token);
    if (!grant) {
      return { refusal: refusal('resume token is unknown, expired or already spent') };
    }
    this.grants.delete(token);
    // The grant names the operation and the identity that earned it. A token
    // presented on another operation, or by another client, buys nothing.
    if (grant.operationId !== operationId || grant.clientId !== clientId) {
      return { refusal: refusal('resume token was not issued for this operation and client') };
    }
    if (grant.budget <= 0) {
      return { refusal: refusal('resume budget for this stream is exhausted') };
    }
    // A consumer cannot claim to have received more than this provider sent:
    // continuing past the cursor would skip values nobody delivered.
    if (afterSequence > grant.cursor) {
      return { refusal: refusal('resume position is ahead of the sequence this provider delivered') };
    }
    return { grant };
  }

  private evictExpired(): void {
    const now = this.now();
    for (const [token, grant] of this.grants) {
      if (grant.expiresAt <= now) this.grants.delete(token);
    }
  }
}

function refusal(message: string): WebSocketDiagnostic {
  return { code: 'client_contract.invalid_resilience', field: 'resume', message };
}

/** One unguessable, URL-safe token. */
function mintResumeToken(): string {
  const raw = new Uint8Array(RESUME_BOUNDS.tokenBytes);
  crypto.getRandomValues(raw);
  let binary = '';
  for (const byte of raw) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
