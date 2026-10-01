import { loader } from '@putnami/web';
import { getNavTree } from '../lib/docs/navigation.server';

// Static: runs at build for the pre-rendered routes it wraps. Reads only the
// generated docs tree (no request data), so it is static-safe.
export default loader()
  .static()
  .handle(async () => {
    try {
      const navItems = await getNavTree();
      return { navItems };
    } catch (_error) {
      return { navItems: [] };
    }
  });
