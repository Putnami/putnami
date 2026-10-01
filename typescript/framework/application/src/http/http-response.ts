import { getHttpStatusText } from './http-status.type';

/** Shared empty header list returned by {@link HttpResponse.rawHeaderEntries} to avoid per-call allocation. */
const EMPTY_HEADERS: readonly [string, string][] = [];

type XMLHttpRequestBodyInit = Blob | BufferSource | FormData | URLSearchParams | string;
type BodyInit = ReadableStream | XMLHttpRequestBodyInit;

type HeadersInit = Record<string, string> | [string, string][];

interface ResponseInit {
  headers?: HeadersInit;
  status?: number;
  statusText?: string;
}

export class HttpResponse {
  private headers?: [string, string][];

  readonly status?;

  readonly statusText?: string;

  private readonly bodyInit?: BodyInit;

  private bodyUsed = false;

  /**
   * The original unserialized data passed to `HttpResponse.json()`.
   * Used by the gRPC plugin to encode binary protobuf directly,
   * avoiding a JSON stringify -> parse round-trip.
   */
  private _rawData?: unknown;

  constructor(bodyInit?: BodyInit, resInit?: ResponseInit) {
    this.status = resInit?.status;
    if (Array.isArray(resInit?.headers)) {
      this.headers = resInit.headers.map(([k, v]) => [k, v]);
    } else if (typeof resInit?.headers === 'object') {
      this.headers = Object.entries(resInit?.headers || {}).map(([k, v]) => [k, v]);
    }

    this.bodyInit = bodyInit;
  }

  /**
   * Returns the original unserialized data if this response was created via
   * `HttpResponse.json()`. Returns `undefined` for non-JSON responses.
   */
  rawData(): unknown {
    return this._rawData;
  }

  get ok(): boolean {
    return !this.status || this.status < 300;
  }

  get redirected(): boolean {
    return !!this.status && this.status >= 300 && this.status < 400 && !!this.getHeader('Location');
  }

  copy(): HttpResponse {
    const rh = new HttpResponse(this.bodyInit, {
      headers: this.headers,
      status: this.status,
      statusText: this.statusText,
    });
    rh._rawData = this._rawData;
    return rh;
  }

  getHeader(name: string): string | undefined {
    return this.headers?.find(([n]) => n === name)?.[1];
  }

  getHeaders(name: string): string[] {
    return this.headers?.filter(([n]) => n === name).map(([_, v]) => v) || [];
  }

  getHeaderEntries(): [string, string][] {
    return this.headers ? [...this.headers] : [];
  }

  /**
   * Internal, non-copying view of the header entries for hot paths (e.g. the
   * security-headers middleware). Callers MUST NOT mutate the returned array;
   * use {@link getHeaderEntries} when a defensive copy is required.
   */
  rawHeaderEntries(): readonly [string, string][] {
    return this.headers ?? EMPTY_HEADERS;
  }

  /** Returns the raw body init value (before Response construction). */
  getBodyInit(): BodyInit | undefined {
    return this.bodyInit;
  }

  get(): Response {
    if (this.bodyUsed) {
      throw new Error('response already built');
    }
    this.bodyUsed = true;

    if (this.redirected) {
      return new Response(undefined, {
        status: this.status,
        headers: [...(this.headers || [])],
      });
    }

    const status = this.status || 200;
    const statusText = this.statusText || getHttpStatusText(status);

    return new Response(this.bodyInit, {
      headers: this.headers || {},
      status,
      statusText,
    });
  }

  /**
   * Set a header, replacing any existing header with the same name.
   * Mutates in-place and returns `this` to allow chaining.
   */
  setHeader(name: string, value: string): HttpResponse {
    if (!this.headers) {
      this.headers = [[name, value]];
      return this;
    }
    // Remove existing entries with this name, then append
    let writeIdx = 0;
    for (let i = 0; i < this.headers.length; i++) {
      if (this.headers[i][0] !== name) {
        this.headers[writeIdx] = this.headers[i];
        writeIdx++;
      }
    }
    this.headers.length = writeIdx;
    this.headers.push([name, value]);
    return this;
  }

  /**
   * Append a header value (allows duplicates).
   * Mutates in-place and returns `this` to allow chaining.
   */
  pushHeader(name: string, value: string): HttpResponse {
    this.headers ??= [];
    this.headers.push([name, value]);
    return this;
  }

  withStatus(status: number, statusText?: string): HttpResponse {
    const rh = new HttpResponse(this.bodyInit, {
      headers: this.headers,
      status,
      statusText: statusText || this.statusText,
    });
    rh._rawData = this._rawData;
    return rh;
  }

  withBody(bodyInit?: BodyInit): HttpResponse {
    return new HttpResponse(bodyInit, {
      headers: this.headers,
      status: this.status,
      statusText: this.statusText,
    });
  }

  /**
   * AMBIGUOUS = 300,
   * MOVED_PERMANENTLY = 301,
   * FOUND = 302,
   * SEE_OTHER = 303,
   * NOT_MODIFIED = 304,
   * TEMPORARY_REDIRECT = 307,
   * PERMANENT_REDIRECT = 308,
   */
  static redirect(url: string, status: 300 | 301 | 302 | 303 | 304 | 307 | 308 = 302): HttpResponse {
    return new HttpResponse(undefined, {
      status: status,
      headers: {
        Location: url,
      },
    });
  }

  static json(data: unknown, init?: ResponseInit): HttpResponse {
    const initHeaders = init?.headers;
    const merged: Record<string, string> = {};
    if (Array.isArray(initHeaders)) {
      for (const [k, v] of initHeaders) merged[k] = v;
    } else if (initHeaders) {
      Object.assign(merged, initHeaders);
    }
    merged['Content-Type'] = 'application/json';
    const rh = new HttpResponse(JSON.stringify(data), {
      status: 200,
      ...init,
      headers: merged,
    });
    rh._rawData = data;
    return rh;
  }

  static notFound(body?: unknown): HttpResponse {
    return HttpResponse.json(body ?? { error: 'Not Found' }, { status: 404 });
  }

  static unauthorized(body?: unknown): HttpResponse {
    return HttpResponse.json(body ?? { error: 'Unauthorized' }, { status: 401 });
  }

  static forbidden(body?: unknown): HttpResponse {
    return HttpResponse.json(body ?? { error: 'Forbidden' }, { status: 403 });
  }

  static internalServerError(body?: unknown): HttpResponse {
    return HttpResponse.json(body ?? { error: 'Internal Server Error' }, { status: 500 });
  }

  static badRequest(body?: unknown): HttpResponse {
    return HttpResponse.json(body ?? { error: 'Bad Request' }, { status: 400 });
  }

  static noContent(): HttpResponse {
    return new HttpResponse(undefined, { status: 204 });
  }
}

// ---------------------------------------------------------------------------
// Standalone factory functions
// ---------------------------------------------------------------------------

/** Create a JSON response. */
export function json(data: unknown, init?: ResponseInit): HttpResponse {
  return HttpResponse.json(data, init);
}

/** Create a 400 Bad Request response. Pass a body to include a JSON payload. */
export function badRequest(body?: unknown): HttpResponse {
  return HttpResponse.badRequest(body);
}

/** Create a 404 Not Found response. Pass a body to include a JSON payload. */
export function notFound(body?: unknown): HttpResponse {
  return HttpResponse.notFound(body);
}

/** Create a 401 Unauthorized response. Pass a body to include a JSON payload. */
export function unauthorized(body?: unknown): HttpResponse {
  return HttpResponse.unauthorized(body);
}

/** Create a 403 Forbidden response. Pass a body to include a JSON payload. */
export function forbidden(body?: unknown): HttpResponse {
  return HttpResponse.forbidden(body);
}

/** Create a 500 Internal Server Error response. Pass a body to include a JSON payload. */
export function internalServerError(body?: unknown): HttpResponse {
  return HttpResponse.internalServerError(body);
}

/** Create a 204 No Content response. */
export function noContent(): HttpResponse {
  return HttpResponse.noContent();
}
