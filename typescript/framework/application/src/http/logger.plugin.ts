import type { Module, Plugin } from '../application';
import { HttpPlugin } from './http.plugin';
import { LoggerMiddleware, type LoggerOptions } from './logger.middleware';

export class LoggerPlugin implements Plugin {
  constructor(private options: LoggerOptions = {}) {}

  async warmup(app: Module): Promise<void> {
    const http = await app.ensurePlugin(HttpPlugin);
    http.use(LoggerMiddleware(this.options));
  }
}

export const logger = (options?: LoggerOptions) => new LoggerPlugin(options);
