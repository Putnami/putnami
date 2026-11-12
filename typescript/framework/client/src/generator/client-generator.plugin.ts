import type { ClientOperationAttribution, Plugin } from '@putnami/application';

/**
 * Plugin that declares the application a client provider: its `openapi()`
 * document names, in each operation's `x-putnami-client`, the project and the
 * feature that produce it.
 *
 * It names no target, output or package, and writes no file. A client project
 * names this provider in its own `generatedFrom` and generates itself from the
 * provider's committed `schema/openapi.json`
 * (tooling/clientgen-extension ADR 0007). The generated client carries each
 * operation's producer into its design declaration.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api({ client: { service: { id: 'users', audience: 'users' }, credentials: {} } }))
 *   .use(openapi({ title: 'Users API', version: '1.0.0' }))
 *   .use(clientGenerator());
 * ```
 */
export class ClientGeneratorPlugin implements Plugin, ClientOperationAttribution {
  readonly name = 'client-generator';
  readonly attributesClientOperations = true;
}

/**
 * Factory function for creating a ClientGeneratorPlugin.
 */
export function clientGenerator(): ClientGeneratorPlugin {
  return new ClientGeneratorPlugin();
}
