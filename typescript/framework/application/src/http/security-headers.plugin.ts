import type { Module, Plugin } from '../application';
import { HttpPlugin } from './http.plugin';
import { type SecurityHeadersOptions, SecurityHeadersMiddleware } from './security-headers.middleware';

export class SecurityHeadersPlugin implements Plugin {
  constructor(private options: SecurityHeadersOptions = {}) {}

  async warmup(app: Module): Promise<void> {
    const http = await app.ensurePlugin(HttpPlugin);
    http.prepend(SecurityHeadersMiddleware(this.options));
  }
}

export const securityHeaders = (options?: SecurityHeadersOptions) => new SecurityHeadersPlugin(options);
