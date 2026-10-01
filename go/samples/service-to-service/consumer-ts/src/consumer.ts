import {
  registerAuditClient,
  registerBlobsClient,
  registerBodyFidelityClient,
  registerCredentialCheckClient,
  registerItemsClient,
  registerQuotesClient,
  registerTenantCheckClient,
  registerWhoamiClient,
} from '@example/go-items-client';
import { application } from '@putnami/application';

/**
 * The consumer side of the cross-language sample: it registers the generated
 * clients of the Go Items provider and lets the framework build them from the
 * `clients` block of its configuration. There is no base URL in this file, no
 * header, no interceptor and no retry policy — the provider declared all of
 * that in its contract, and the binding supplies the values.
 *
 * It serves nothing. `logger()` is deliberately absent: that plugin ensures the
 * HTTP plugin, which would make a client-only workload bind a port.
 */
export function app() {
  const instance = application().feature({
    id: 'items/consume',
    name: 'Cross-language item consumption',
    outcome: 'A TypeScript consumer calls the Go Items provider through generated clients',
    owner: 'samples',
  });

  registerItemsClient(instance);
  registerCredentialCheckClient(instance);
  registerBodyFidelityClient(instance);
  // Opaque JSON: values the provider carries without interpreting them.
  registerAuditClient(instance);
  registerBlobsClient(instance);
  registerQuotesClient(instance);
  // The two identity operations: one alternative requiring two credentials at
  // once, and one carrying the caller's own forwarded user identity.
  registerTenantCheckClient(instance);
  registerWhoamiClient(instance);
  return instance;
}
