import { loader } from '@putnami/web';
import { getNavTree } from '../../lib/docs/navigation.server';

// Static: runs at build for the pre-rendered docs routes (reads the generated
// docs tree, no request data).
export default loader()
  .static()
  .handle(async () => {
    const navItems = await getNavTree();
    return { navItems };
  });
