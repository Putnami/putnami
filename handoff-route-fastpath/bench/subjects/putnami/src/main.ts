import { api, application, http, logger, platform } from '@putnami/application';
import { sql } from '@putnami/database';
import { VARIANT } from './data';
import * as health from './api/health/get';
import * as getOne from './api/tasks/[id]/get';
import * as list from './api/tasks/get';
import * as create from './api/tasks/post';

// Ablation switches (BENCH_OFF, separated by + or ,): logger, platform,
// securityHeaders, originGuard, deadline, httpMethods, bodyLimit.
const off = new Set((process.env.BENCH_OFF ?? '').split(/[,+]/).filter(Boolean));

const serverOptions: Record<string, unknown> = { port: Number(process.env.PORT ?? 3906) };
if (off.has('securityHeaders')) serverOptions.securityHeaders = false;
if (off.has('originGuard')) serverOptions.originGuard = false;
if (off.has('deadline')) serverOptions.requestTimeoutMs = 0;
if (off.has('httpMethods')) serverOptions.httpMethods = false;
if (off.has('bodyLimit')) serverOptions.maxBodySizeBytes = 0;

const routes = api({ autoScan: false })
  .register('/health', health, 'GET')
  .register('/tasks/[id]', getOne, 'GET')
  .register('/tasks', list, 'GET')
  .register('/tasks', create, 'POST');

export const app = () => {
  let a = application().use(http(serverOptions as never));
  if (!off.has('logger')) a = a.use(logger());
  if (VARIANT !== 'raw') a = a.use(sql());
  if (!off.has('platform')) a = a.use(platform());
  return a.use(routes);
};
