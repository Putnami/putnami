import { incCounter, observeHistogram } from '@putnami/application';
import { metricServiceName } from '../runtime/metric-name';
import type { ClientRequest, ClientResponse, Interceptor } from '../runtime/transport.type';

/**
 * Creates a telemetry interceptor that records metrics for every client call.
 *
 * Recorded metrics:
 * - Counter: `client.{service}.request` — total requests
 * - Counter: `client.{service}.{status}` — per status code
 * - Counter: `client.{service}.error` — failed requests
 * - Histogram: `client.{service}.duration` — response time in ms
 *
 * All metrics are no-ops when telemetry is not initialized.
 */
export function telemetryInterceptor(serviceName: string): Interceptor {
  const metricName = metricServiceName(serviceName);

  // Pre-compute fixed metric keys to avoid string allocation on every request
  const requestKey = `client.${metricName}.request`;
  const errorKey = `client.${metricName}.error`;
  const durationKey = `client.${metricName}.duration`;
  const statusPrefix = `client.${metricName}.`;

  return async (request: ClientRequest, next: (req: ClientRequest) => Promise<ClientResponse>) => {
    const start = performance.now();
    incCounter(requestKey);

    try {
      const response = await next(request);
      const duration = performance.now() - start;

      incCounter(`${statusPrefix}${response.status}`);
      observeHistogram(durationKey, duration);

      return response;
    } catch (error) {
      const duration = performance.now() - start;

      incCounter(errorKey);
      observeHistogram(durationKey, duration);

      throw error;
    }
  };
}
