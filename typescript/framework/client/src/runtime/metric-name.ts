/** Convert a contract service identity to a bounded metric-key component. */
export function metricServiceName(serviceName: string): string {
  if (!serviceName) throw new Error('serviceName must not be empty');
  const normalized = serviceName.replace(/[^A-Za-z0-9_-]+/g, '_');
  if (normalized === serviceName) return normalized;
  let hash = 2_166_136_261;
  for (const character of serviceName) {
    hash ^= character.charCodeAt(0);
    hash = Math.imul(hash, 16_777_619) >>> 0;
  }
  return `${normalized || 'service'}_${hash.toString(36)}`;
}
