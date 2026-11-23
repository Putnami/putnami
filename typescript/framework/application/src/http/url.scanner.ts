export class UrlScanner {
  constructor(
    private url: string,
    private headers?: Headers,
  ) {
    //
  }

  private _secured?: boolean;
  get secured() {
    if (this._secured !== undefined) {
      return this._secured;
    }

    if (this.url.startsWith('https')) {
      this._secured = true;
    } else {
      // Plaintext `http://` URL or a relative path. Behind a TLS-terminating
      // proxy Bun receives the request over plain HTTP and rewrites the URL to
      // `http://...`, so the direct scheme is not authoritative. Honour the
      // `x-forwarded-proto` hop header (the proxy's view of the client leg) and
      // treat the request as secure when it reports https. This only upgrades
      // the perceived scheme (e.g. adds the cookie `Secure` flag), so a spoofed
      // header fails safe rather than open.
      this._secured = this.headers?.get('x-forwarded-proto') === 'https';
    }

    return this._secured;
  }

  private _host?: string;
  get host() {
    if (this._host) {
      return this._host;
    }

    if (this.url.startsWith('http')) {
      const doubleSlashIndex = this.url.indexOf('//');
      if (doubleSlashIndex !== -1) {
        const pathStartIndex = this.url.indexOf('/', doubleSlashIndex + 2);
        if (pathStartIndex !== -1) {
          this._host = this.url.slice(doubleSlashIndex + 2, pathStartIndex);
        } else {
          // http://example.com (no trailing slash)
          const queryStartIndex = this.url.indexOf('?');
          if (queryStartIndex !== -1) {
            this._host = this.url.slice(doubleSlashIndex + 2, queryStartIndex);
          } else {
            this._host = this.url.slice(doubleSlashIndex + 2);
          }
        }
      }
    }

    if (!this._host) {
      this._host = this.headers?.get('host') || this.headers?.get('x-forwarded-host') || '';
    }

    return this._host;
  }

  private _domain?: string;
  get domain() {
    if (this._domain) {
      return this._domain;
    }

    this._domain = `http${this.secured ? 's' : ''}://${this.host}`;
    return this._domain;
  }

  private _pathname?: string;
  /**
   * The URL's path with its leading slash and without its query: the form
   * the router matches, read without slicing the URL again.
   */
  get pathname(): string {
    if (this._pathname !== undefined) {
      return this._pathname;
    }

    let start = 0;
    // If absolute, skip scheme and host
    if (this.url.startsWith('http')) {
      const doubleSlashIndex = this.url.indexOf('//');
      if (doubleSlashIndex !== -1) {
        const nextSlash = this.url.indexOf('/', doubleSlashIndex + 2);
        if (nextSlash === -1) {
          // http://example.com and http://example.com?foo have the path /
          this._pathname = '/';
          return this._pathname;
        }
        start = nextSlash;
      }
    }

    const queryIndex = this.url.indexOf('?', start);
    const end = queryIndex === -1 ? this.url.length : queryIndex;
    this._pathname = start === 0 && end === this.url.length ? this.url : this.url.slice(start, end);
    return this._pathname;
  }

  private _path?: string;
  get path() {
    if (this._path !== undefined) {
      return this._path;
    }

    // Strip leading slash — callers expect bare path segments (e.g. 'test' not '/test')
    const pathname = this.pathname;
    this._path = pathname.startsWith('/') ? pathname.slice(1) : pathname;
    return this._path;
  }

  private _query?: string;
  get query() {
    if (this._query) {
      return this._query;
    }

    const queryIndex = this.url.indexOf('?');
    if (queryIndex !== -1) {
      this._query = this.url.slice(queryIndex);
    } else {
      this._query = '';
    }

    return this._query;
  }
}
