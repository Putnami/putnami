import { api, logger, platform } from '@putnami/application';
import { app as consumer } from './consumer';

/**
 * The served consumer: the client-only application of `./consumer`, plus the
 * HTTP surface a composition reaches it through — the platform probes and the
 * routes under `src/api`, one of which calls the Go provider through its
 * generated client.
 *
 * `./consumer` stays client-only on purpose: the cross-language test starts it
 * in-process, and a listener there would bind a port the test never uses.
 * `putnami compose @example/go-items-consumer` serves this one, with the
 * provider's URL bound under `clients.services.items`.
 */
export const app = () => consumer().use(logger()).use(platform()).use(api());
