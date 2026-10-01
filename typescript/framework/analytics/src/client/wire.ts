/**
 * The browser side of the wire contract (`protocols/analytics`, body §A.2).
 *
 * Types only: this module compiles to nothing, so the tracker bundle pays no
 * bytes for it. The server twin lives in `src/server/sanitize/sanitizer.ts`
 * and is deliberately *not* imported here — a browser file that reaches under
 * `src/server/**` drags the protocol vocabulary, `node:buffer`, and everything
 * behind them into a bundle that must stay under 4 KiB.
 */

/** The five campaign parameters, each absent or a bounded string. */
export interface WireUtm {
  source?: string;
  medium?: string;
  campaign?: string;
  content?: string;
  term?: string;
}

/** The page a `page_view` describes. */
export interface WirePage {
  /** Location pathname; starts with `/`, no query, no fragment. */
  path: string;
  /** Matched route pattern in file-route form, when the client knows one. */
  route?: string;
  /** Absolute `http(s)` URL or an app-relative path, query and fragment stripped. */
  referrer?: string;
  /** Campaign parameters read from the landing URL. */
  utm?: WireUtm;
}

/** One event as it travels to `POST /_putnami/analytics/events`. */
export interface WireEvent {
  /** UUID v7; stable across every retry and every engagement re-send. */
  eventId: string;
  /** The only two names the wire accepts. */
  name: 'page_view' | 'action';
  /** RFC 3339 with milliseconds and a literal `Z`. */
  clientTs: string;
  /** Position of the event inside its session. */
  seq: number;
  /** UUID v7 of the client-owned session. */
  sessionId: string;
  /** Visible time accumulated on the current view, in milliseconds. */
  engagementMs?: number;
  /** Bucket of `window.innerWidth`. */
  viewportClass?: 'xs' | 'sm' | 'md' | 'lg' | 'xl';
  /** BCP 47 tag of the browser language. */
  language?: string;
  /** Present on `page_view`. */
  page?: WirePage;
  /** Present on `action`: the declared action name. */
  action?: string;
  /** Present on `action`: the declared properties. */
  props?: Record<string, string | number | boolean>;
}

/** The batch envelope (body §A.1). */
export interface WireBatch {
  protocolVersion: number;
  sentAt: string;
  events: WireEvent[];
}

/**
 * What the server tells the browser about the page it just rendered.
 *
 * Structurally identical to `AnalyticsBootstrap` in
 * `src/server/http/bootstrap.ts`; `test/tracker.runtime.test.ts` pins the two
 * copies against each other. The list is exhaustive: no `user_id`, no visitor
 * id, no secret.
 */
export interface ClientBootstrap {
  /** The server-minted page-view event id the tracker re-sends enrichment for. */
  pv: string;
  /** The matched route pattern of this view. */
  route: string;
  /** Where the tracker POSTs its batches. */
  endpoint: string;
  /** The application name. */
  app: string;
  /** The deployment environment. */
  env: string;
  /** The application version, or null. */
  version: string | null;
  /** The action names the server accepts. */
  declared: string[];
}

/** Records one declared action. */
export type TrackFn = (name: string, props?: Record<string, string | number | boolean>) => void;
