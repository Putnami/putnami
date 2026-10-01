import { $ } from 'bun';
import { useConfig, type InferConfig } from '@putnami/runtime';
import { database } from '../../src/factory';
import { PostgresConfig } from '../../src/postgres/config';

/**
 * Checks if a Docker container is running
 */
async function isPostgresRunning(db: string): Promise<boolean> {
  try {
    const sql = await database(db);
    await sql`SELECT 1`;
    return true;
  } catch (_e) {
    return false;
  }
}

function containerName(db: string): string {
  const config = useConfig(PostgresConfig, { path: `database.${db}` });
  return `pg-test-${config.database}`;
}

/**
 * How long to wait for Postgres to answer after the container starts.
 *
 * A warm container answers in a second or two. A cold one has to pull
 * `postgres:17-alpine` first, which is what the old 15s budget did not cover:
 * the wait expired mid-pull, the suite failed, and the retry then collided with
 * the container this call had already named. Sixty seconds covers the pull; a
 * caller that starts a container has to allow at least this much in its own
 * hook timeout, which is why the constant is exported.
 */
const POSTGRES_READY_TIMEOUT_MS = 60_000;

/**
 * The timeout a `beforeAll` calling `ensurePostgresContainer` has to declare.
 *
 * Bun's default hook timeout is 5s, so a hook that starts a container reports
 * "test timed out" long before the wait above can succeed — the failure names
 * the test rather than the container it was waiting for. The extra budget
 * covers the schema and fixture setup these hooks do after Postgres answers.
 */
export const POSTGRES_SETUP_TIMEOUT_MS = POSTGRES_READY_TIMEOUT_MS + 15_000;

const POSTGRES_POLL_INTERVAL_MS = 500;

async function waitForPostgres(
  db: string,
  maxAttempts = POSTGRES_READY_TIMEOUT_MS / POSTGRES_POLL_INTERVAL_MS,
): Promise<void> {
  for (let i = 0; i < maxAttempts; i++) {
    try {
      const r = await isPostgresRunning(db);
      if (r) {
        return;
      }
    } catch (_e) {
      // ignore
    }
    await new Promise((resolve) => setTimeout(resolve, POSTGRES_POLL_INTERVAL_MS));
  }
  throw new Error(`PostgreSQL container ${containerName(db)} failed to become ready`);
}

/** Reports whether a container of this name exists, running or stopped. */
async function containerExists(name: string): Promise<boolean> {
  const result = await $`docker ps --all --quiet --filter ${`name=^${name}$`}`.quiet().nothrow();
  return result.exitCode === 0 && result.text().trim() !== '';
}

/** Renders a failed `docker` invocation with the stderr that says why. */
function dockerFailure(args: readonly string[], result: { text(): string; stderr: Buffer }): Error {
  // docker writes its failures to stderr; .text() is stdout only.
  const stderr = result.stderr.toString().trim();
  return new Error(`Failed to run '${args.join(' ')}': ${stderr || result.text().trim() || 'no output'}`);
}

export async function isDockerAvailable(): Promise<boolean> {
  try {
    const result = await $`docker info`.quiet().nothrow();
    return result.exitCode === 0;
  } catch {
    return false;
  }
}

export async function ensurePostgresContainer(db: string): Promise<InferConfig<typeof PostgresConfig>> {
  if (process.env.NODE_ENV !== 'test') {
    return useConfig(PostgresConfig);
  }

  const config = useConfig(PostgresConfig, { path: `database.${db}` });

  // Anything already answering on this datasource wins, whatever started it: a
  // container from an earlier run, a Postgres the developer runs themselves, or
  // the service container .github/workflows/contributor-ci.yml publishes. In CI
  // this is the only branch that ever runs, so the gate never pulls an image.
  const running = await isPostgresRunning(db);
  if (running) {
    return config;
  }

  const name = containerName(db);

  // The name is fixed, so a container that exists but is not answering is the
  // wreckage of an interrupted run — a pull that was killed, or a start that
  // outlived the wait below. `docker run` would refuse the name and report a
  // conflict, which reads as "Docker is broken" rather than "restart this".
  // Starting it is idempotent: Docker accepts an already-running container.
  if (await containerExists(name)) {
    const startArgs = ['docker', 'start', name];
    const startResult = await $`${startArgs}`.quiet().nothrow();
    if (startResult.exitCode !== 0) {
      throw dockerFailure(startArgs, startResult);
    }
    await waitForPostgres(db);
    return config;
  }

  const args: string[] = [
    'docker',
    'run',
    '-d',
    '--name',
    name,
    '-e',
    `POSTGRES_USER=${config.user}`,
    '-e',
    `POSTGRES_PASSWORD=${config.password}`,
    '-e',
    `POSTGRES_DB=${config.database}`,
    '-p',
    `${config.port}:5432`,
    'postgres:17-alpine',
  ];
  const runResult = await $`${args}`.quiet().nothrow();

  if (runResult.exitCode !== 0) {
    throw dockerFailure(args, runResult);
  }

  await waitForPostgres(db);

  return config;
}
