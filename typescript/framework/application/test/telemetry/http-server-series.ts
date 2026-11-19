import type { MetricsBucket } from '../../src/telemetry/telemetry.collector';

/** The labels of one HTTP server series, as the telemetry middleware records them. */
export interface HttpSeriesLabels {
  method: string;
  route: string;
  status: number;
}

/** The labels of every http.server.request_count series in the buckets, with its total. */
export function requestCounts(buckets: readonly MetricsBucket[]): Array<HttpSeriesLabels & { count: number }> {
  const totals = new Map<string, HttpSeriesLabels & { count: number }>();
  for (const bucket of buckets) {
    for (const series of bucket.counterSeries ?? []) {
      if (series.name !== 'http.server.request_count') continue;
      const labels = {
        method: String(series.attributes['http.method']),
        route: String(series.attributes['http.route']),
        status: Number(series.attributes['http.status_code']),
      };
      const key = JSON.stringify(labels);
      const current = totals.get(key) ?? { ...labels, count: 0 };
      current.count += series.value;
      totals.set(key, current);
    }
  }
  return [...totals.values()];
}

/** How many requests the buckets count under exactly these labels. */
export function requestCount(buckets: readonly MetricsBucket[], labels: HttpSeriesLabels): number {
  return (
    requestCounts(buckets).find(
      (series) => series.method === labels.method && series.route === labels.route && series.status === labels.status,
    )?.count ?? 0
  );
}
