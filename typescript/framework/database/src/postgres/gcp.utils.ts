/**
 * GCP identity helpers used by the Postgres provider when `user` or
 * `password` is omitted. The metadata server is the production source
 * (Cloud Run, GCE, Cloud Build); `gcloud` is the dev-laptop fallback.
 */

const METADATA_BASE = 'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default';
const METADATA_HEADERS = { 'Metadata-Flavor': 'Google' } as const;

/**
 * Fetch a short-lived OAuth access token for the active service account
 * from the GCP metadata server. Used as the Postgres password when
 * connecting through the Cloud Run-mounted Cloud SQL Unix socket.
 */
export async function getGcpAccessToken(): Promise<string> {
  const response = await fetch(`${METADATA_BASE}/token`, { headers: METADATA_HEADERS });
  if (!response.ok) {
    throw new Error(`metadata server token: ${response.status} ${response.statusText}`);
  }
  const data = (await response.json()) as { access_token: string };
  return data.access_token;
}

/**
 * Fetch the active service account email from the GCP metadata server.
 * Throws if not running on a GCP workload (the hostname does not resolve
 * off-cloud).
 */
async function getGcpServiceAccount(): Promise<string> {
  const response = await fetch(`${METADATA_BASE}/email`, { headers: METADATA_HEADERS });
  if (!response.ok) {
    throw new Error(`metadata server email: ${response.status} ${response.statusText}`);
  }
  return (await response.text()).trim();
}

/**
 * Return the active gcloud account, used as the dev-laptop fallback when
 * the metadata server is unreachable.
 */
async function getGcloudActiveAccount(): Promise<string> {
  const proc = Bun.spawn({
    cmd: ['gcloud', 'config', 'get-value', 'account'],
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const exitCode = await proc.exited;
  const stdout = (await new Response(proc.stdout).text()).trim();
  if (exitCode !== 0) {
    const stderr = (await new Response(proc.stderr).text()).trim();
    throw new Error(`gcloud exited ${exitCode}: ${stderr || '(no stderr)'}`);
  }
  if (!stdout || stdout === '(unset)') {
    throw new Error('no active gcloud account');
  }
  return stdout;
}

/**
 * Resolve the active IAM principal for the current process. Tries the GCP
 * metadata server first (Cloud Run, GCE, Cloud Build); falls back to the
 * gcloud CLI on a dev laptop.
 */
export async function resolveGcpIdentity(): Promise<string> {
  const errors: string[] = [];
  try {
    return await getGcpServiceAccount();
  } catch (err) {
    errors.push(`metadata: ${err instanceof Error ? err.message : String(err)}`);
  }
  try {
    return await getGcloudActiveAccount();
  } catch (err) {
    errors.push(`gcloud: ${err instanceof Error ? err.message : String(err)}`);
  }
  throw new Error(
    `database: cannot auto-resolve user — set 'user' explicitly, run on a workload with the GCP metadata server, or 'gcloud auth login'. Tried: ${errors.join('; ')}`,
  );
}
