import type { Module, Plugin } from '../application';
import { HttpPlugin } from './http.plugin';
import { TraceMiddleware } from './trace.middleware';

export class TracePlugin implements Plugin {
  async warmup(app: Module): Promise<void> {
    const http = await app.ensurePlugin(HttpPlugin);
    // Insert trace middleware at the very beginning to ensure context is set early
    http.prepend(TraceMiddleware());
  }
}

export const trace = () => new TracePlugin();
