/**
 * Support-catalog page plugin.
 *
 * Publishes `/docs/support` and `/docs/languages` as GENERATED pages whose only
 * source of truth for a status is the reviewed `putnami.support.json` at the
 * workspace root. No package or protocol status is hand-written anywhere on
 * this site, so the published commitment and the reviewed catalog cannot drift.
 *
 * The write itself lives in `../lib/support/publish` because the docs
 * static-path callback performs the same idempotent write earlier in the build
 * — see that module for why it happens twice.
 *
 * @example
 * ```typescript
 * application().use(supportCatalog())
 * ```
 */
import type { GenerateResult, Plugin } from '@putnami/application';
import { publishSupportPage } from '../lib/support/publish';

export {
  LANGUAGES_PAGE_DIST_PATH,
  LANGUAGES_SECTION,
  SUPPORT_PAGE_DIST_PATH,
  SUPPORT_SECTION,
} from '../lib/support/publish';

class SupportCatalogPlugin implements Plugin {
  generate(): GenerateResult {
    return { assets: publishSupportPage().assets };
  }
}

export function supportCatalog(): SupportCatalogPlugin {
  return new SupportCatalogPlugin();
}
