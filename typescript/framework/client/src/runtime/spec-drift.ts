import { getLogger } from '@putnami/utils';
import { computeSpecHash } from './spec-hash';

const logger = getLogger('client');

/**
 * Spec drift detection — verifies that a generated client is still
 * compatible with the live service's API spec.
 *
 * At startup, fetches the service's spec (OpenAPI or Proto) and compares
 * its hash to the one embedded in the generated client. If they differ,
 * logs a loud warning indicating the client may be out of date.
 *
 * @param baseUrl - The service base URL
 * @param clientSpecHash - The spec hash embedded in the generated client at generation time
 * @param transport - The transport mode ('http' | 'connect')
 */
export async function checkSpecDrift(
  baseUrl: string,
  clientSpecHash: string,
  transport: 'http' | 'connect',
): Promise<void> {
  const specUrl = transport === 'connect' ? `${baseUrl}/_/api.proto` : `${baseUrl}/_/openapi.json`;

  try {
    const response = await fetch(specUrl, {
      method: 'GET',
      signal: AbortSignal.timeout(5000),
    });

    if (!response.ok) {
      logger.warn(
        `[client:spec-drift] Could not fetch spec from ${specUrl} (HTTP ${response.status}). ` +
          'Spec drift detection skipped.',
      );
      return;
    }

    const content = await response.text();
    const liveHash = await computeSpecHash(content);

    if (liveHash !== clientSpecHash) {
      logger.warn(
        `[client:spec-drift] API spec has changed since this client was generated!\n` +
          `  Client was built from: ${clientSpecHash}\n` +
          `  Live service spec:     ${liveHash}\n` +
          `  Service:               ${baseUrl}\n` +
          `  Action: Regenerate the client with \`putnami build\` to pick up the latest API.`,
      );
    }
  } catch (error) {
    // Don't let drift check failures prevent startup
    logger.warn(
      `[client:spec-drift] Failed to check spec drift for ${baseUrl}: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
}
