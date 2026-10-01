import { endpoint, Uuid } from '../../../../../../src';

export default endpoint()
  .params({ id: Uuid })
  .returns({ id: String })
  .handle((ctx) => ({ id: ctx.params.id }));
