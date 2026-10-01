import { loader } from '@putnami/web';
import { getBuildInfo } from '@putnami/utils';
import { getNavTree } from '../lib/docs/navigation.server';

// Static: runs at build for the pre-rendered routes it wraps. Reads only the
// generated docs tree and the build's version stamp (no request data), so it is
// static-safe.
export default loader()
  .static()
  .handle(async () => {
    const version = getBuildInfo()?.version;
    try {
      const navItems = await getNavTree();
      return { navItems, version };
    } catch (_error) {
      return { navItems: [], version };
    }
  });
