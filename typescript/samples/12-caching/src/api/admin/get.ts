import { endpoint } from '@putnami/application';
import { metrics } from '../../metrics';

// GET /admin — Cache hit/miss statistics (application-level instrumentation)
//
// Demo-only: unauthenticated so the sample runs standalone. Real
// applications must gate admin/management surfaces with `.secure()`.
export const GET = endpoint().handle(() => ({
  cache: metrics.snapshot(),
}));
