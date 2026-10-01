/** Canonical operation identity shared by contract projection and live stream admission. */
export function clientOperationId(method: string, path: string): string {
  const segments = path
    .split('/')
    .filter(Boolean)
    .map((segment) => {
      const paramMatch = segment.match(/^\[(.+)]$/);
      if (paramMatch) return `_${paramMatch[1]}`;
      return segment.charAt(0).toUpperCase() + segment.slice(1);
    });
  return `${method.toLowerCase()}${segments.join('')}`;
}
