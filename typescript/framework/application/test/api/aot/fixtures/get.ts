import { Int } from '@putnami/runtime';
import { endpoint } from '../../../../src/api/route/endpoint';

// Fixture endpoint used by codegen-emit.test.ts to exercise build-time AOT emission.
export default endpoint()
  .query({ page: Int, limit: Int })
  .handle((ctx) => ({ q: ctx.queryParams() }));
