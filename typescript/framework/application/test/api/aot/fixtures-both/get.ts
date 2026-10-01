import { Int, Uuid } from '@putnami/runtime';
import { endpoint } from '../../../../src/api/route/endpoint';

// default export: a Uuid query (NOT inlineable). If the codegen wrongly resolves
// `default` first, it would emit no validator.
export default endpoint()
  .query({ a: Uuid })
  .handle(() => ({ ok: true }));

// GET named export: an Int query (inlineable). This is the endpoint runtime
// register() actually uses (method export first), so the codegen must too.
export const GET = endpoint()
  .query({ page: Int })
  .handle((ctx) => ({ q: ctx.queryParams() }));
