import { endpoint } from '../../../../src';

export default endpoint()
  .returns({ items: String })
  .handle(() => ({ items: '[]' }));
