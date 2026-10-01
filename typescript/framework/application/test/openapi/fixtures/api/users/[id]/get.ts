import { endpoint, Uuid } from '../../../../../../src';

export default endpoint()
  .params({ id: Uuid })
  .returns({ id: String, name: String })
  .handle((ctx) => ({ id: ctx.params.id, name: 'Test User' }));
