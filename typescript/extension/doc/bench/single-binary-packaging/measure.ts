#!/usr/bin/env bun
/**
 * Process-level measurements for the single-binary packaging bench.
 *
 * Reads a variant spec file (JSON, written by `bench.sh build`), then for every
 * variant measures:
 *
 * - cold start: wall time from spawn to the first successful HTTP response
 *   (the Cloud Run scale-from-zero proxy), repeated N times on fresh ports;
 * - RSS at idle (process tree, /proc), after the server reports ready;
 * - RSS under load and peak RSS sampled while an HTTP + WebSocket load runs;
 * - HTTP throughput of the load phase (indicative only — it is a static-file
 *   route, and the point is the memory it costs, not the ops/s).
 *
 * Everything is same-host comparable only. Absolute numbers move with the
 * machine; the ratios between rows are the stable signal.
 *
 * Usage:
 *   bun measure.ts <variants.json> [--out result.json]
 */
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';

interface VariantSpec {
  /** Row label in the report. */
  name: string;
  /** argv of the server process. */
  cmd: string[];
  /** Working directory (decides `PWD`, hence `getProjectRoot()`). */
  cwd: string;
  /** Extra environment on top of a minimal production env. */
  env?: Record<string, string>;
}

interface BenchSpec {
  variants: VariantSpec[];
  /** First port to use; each spawn takes the next one (avoids TIME_WAIT reuse). */
  basePort: number;
  /** Cold-start repetitions per variant. */
  coldStarts: number;
  /** HTTP requests in the load phase. */
  httpRequests: number;
  /** In-flight HTTP requests in the load phase. */
  httpConcurrency: number;
  /** Concurrent WebSocket clients in the load phase. */
  wsClients: number;
  /** Chat messages each WebSocket client sends (every one is broadcast to all). */
  wsMessages: number;
  /** Path used as the readiness probe and the HTTP load target. */
  probePath: string;
}

interface VariantResult {
  name: string;
  coldStartMs: { samples: number[]; min: number; median: number; max: number };
  idleRssKb: number | null;
  loadedRssKb: number | null;
  peakRssKb: number | null;
  httpOps: number;
  httpOpsPerSec: number;
  httpErrors: number;
  wsMessagesReceived: number;
  wsErrors: number;
}

const specPath = process.argv[2];
if (!specPath) {
  process.stderr.write('usage: bun measure.ts <variants.json> [--out result.json]\n');
  process.exit(2);
}
const outIndex = process.argv.indexOf('--out');
const outPath = outIndex > 0 ? process.argv[outIndex + 1] : undefined;
const spec = JSON.parse(readFileSync(specPath, 'utf8')) as BenchSpec;
if (!Number.isInteger(spec.coldStarts) || spec.coldStarts < 1) {
  // stats() over zero samples has no meaning — fail before measuring anything.
  process.stderr.write(`coldStarts must be a positive integer, got ${spec.coldStarts}\n`);
  process.exit(2);
}

let nextPort = spec.basePort;

/**
 * Next port nothing else on the host is already listening on. A bench that
 * silently talks to whatever owns the port it wanted reports nonsense, so bind
 * first and only hand out ports that were actually free.
 */
function takePort(): number {
  for (;;) {
    const port = nextPort++;
    try {
      const probe = Bun.listen({ hostname: '127.0.0.1', port, socket: { data() {} } });
      probe.stop(true);
      return port;
    } catch {
      process.stderr.write(`port ${port} is busy, skipping\n`);
    }
  }
}

/** Resident set size of one process, in KiB, or null when it is gone. */
function rssKb(pid: number): number | null {
  try {
    const status = readFileSync(`/proc/${pid}/status`, 'utf8');
    const match = /^VmRSS:\s+(\d+)\s+kB$/m.exec(status);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

/** pid -> children, from /proc. Used to charge a whole process tree to a row. */
function childMap(): Map<number, number[]> {
  const map = new Map<number, number[]>();
  for (const entry of readdirSync('/proc')) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const stat = readFileSync(`/proc/${entry}/stat`, 'utf8');
      // comm (field 2) can contain spaces and parentheses: parse after the last ')'.
      const tail = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
      const ppid = Number(tail[1]);
      const list = map.get(ppid);
      if (list) list.push(Number(entry));
      else map.set(ppid, [Number(entry)]);
    } catch {
      // Process exited between readdir and read — it contributes nothing.
    }
  }
  return map;
}

/**
 * RSS of a process and every descendant. `bun run` may fork; a compiled binary
 * does not — summing the tree keeps the comparison on the same footing.
 */
function rssKbTree(root: number): number | null {
  if (process.platform !== 'linux') return null;
  const children = childMap();
  const stack = [root];
  let total = 0;
  let seen = false;
  while (stack.length > 0) {
    const pid = stack.pop() as number;
    const rss = rssKb(pid);
    if (rss !== null) {
      total += rss;
      seen = true;
    }
    for (const child of children.get(pid) ?? []) stack.push(child);
  }
  return seen ? total : null;
}

function baseEnv(variant: VariantSpec, port: number): Record<string, string> {
  return {
    PATH: process.env['PATH'] ?? '/usr/bin:/bin',
    HOME: process.env['HOME'] ?? '/root',
    NODE_ENV: 'production',
    PORT: String(port),
    PWD: variant.cwd,
    PUTNAMI_PROJECT_ROOT: variant.cwd,
    ...(variant.env ?? {}),
  };
}

async function waitForOk(port: number, path: string, startedAt: number, timeoutMs: number): Promise<number> {
  const url = `http://127.0.0.1:${port}${path}`;
  const deadline = startedAt + timeoutMs;
  for (;;) {
    try {
      const response = await fetch(url);
      if (response.ok) {
        await response.arrayBuffer();
        return performance.now() - startedAt;
      }
      await response.arrayBuffer();
    } catch {
      // Connection refused while the process is still booting.
    }
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${url}`);
    await Bun.sleep(1);
  }
}

interface RunningServer {
  proc: Bun.Subprocess;
  port: number;
  readyMs: number;
}

async function start(variant: VariantSpec): Promise<RunningServer> {
  const port = takePort();
  const startedAt = performance.now();
  const proc = Bun.spawn({
    cmd: variant.cmd,
    cwd: variant.cwd,
    env: baseEnv(variant, port),
    stdout: 'ignore',
    stderr: 'pipe',
  });
  // Drain stderr continuously, keeping only a tail for diagnostics: a piped
  // stream nobody reads fills its ~64 KiB buffer and then blocks the server
  // mid-measurement, which would deadlock the load phase.
  let stderrTail = '';
  void (async () => {
    const reader = (proc.stderr as ReadableStream<Uint8Array>).getReader();
    const decoder = new TextDecoder();
    for (;;) {
      const chunk = await reader.read().catch(() => ({ done: true as const, value: undefined }));
      if (chunk.done) return;
      stderrTail = (stderrTail + decoder.decode(chunk.value)).slice(-2000);
    }
  })();
  try {
    const readyMs = await waitForOk(port, spec.probePath, startedAt, 30_000);
    return { proc, port, readyMs };
  } catch (error) {
    proc.kill(9);
    await proc.exited;
    throw new Error(`${variant.name}: ${(error as Error).message}\n${stderrTail}`);
  }
}

async function stop(server: RunningServer): Promise<void> {
  server.proc.kill();
  const exited = await Promise.race([server.proc.exited, Bun.sleep(3000).then(() => 'timeout' as const)]);
  if (exited === 'timeout') server.proc.kill(9);
  await server.proc.exited;
}

async function httpLoad(
  port: number,
  path: string,
  total: number,
  concurrency: number,
): Promise<{ ops: number; errors: number; opsPerSec: number }> {
  const url = `http://127.0.0.1:${port}${path}`;
  let issued = 0;
  let ops = 0;
  let errors = 0;
  const startedAt = performance.now();
  await Promise.all(
    Array.from({ length: concurrency }, async () => {
      for (;;) {
        if (issued >= total) return;
        issued++;
        try {
          const response = await fetch(url);
          await response.arrayBuffer();
          if (response.ok) ops++;
          else errors++;
        } catch {
          errors++;
        }
      }
    }),
  );
  const elapsed = performance.now() - startedAt;
  return { ops, errors, opsPerSec: elapsed > 0 ? (ops / elapsed) * 1000 : 0 };
}

/**
 * Chat load: every client joins, then sends messages that the sample broadcasts
 * to all connections — so message volume is quadratic in `clients`, which is the
 * point (it is the memory shape of a real-time server, not a throughput claim).
 */
async function wsLoad(port: number, clients: number, messages: number): Promise<{ received: number; errors: number }> {
  let received = 0;
  let errors = 0;
  const sockets: WebSocket[] = [];
  await Promise.all(
    Array.from(
      { length: clients },
      (_unused, index) =>
        new Promise<void>((resolve) => {
          const socket = new WebSocket(`ws://127.0.0.1:${port}/chat`);
          sockets.push(socket);
          socket.onmessage = () => {
            received++;
          };
          socket.onerror = () => {
            errors++;
            resolve();
          };
          socket.onopen = () => {
            socket.send(JSON.stringify({ type: 'join', username: `bench-${index}` }));
            resolve();
          };
        }),
    ),
  );
  for (let round = 0; round < messages; round++) {
    for (const socket of sockets) {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'message', text: `round ${round}` }));
      } else {
        errors++;
      }
    }
    // Let the broadcast fan-out drain instead of queueing an unbounded backlog.
    await Bun.sleep(5);
  }
  await Bun.sleep(250);
  for (const socket of sockets) socket.close();
  await Bun.sleep(100);
  return { received, errors };
}

function stats(samples: number[]): { samples: number[]; min: number; median: number; max: number } {
  const sorted = [...samples].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  const median = sorted.length % 2 === 0 ? (sorted[mid - 1] + sorted[mid]) / 2 : sorted[mid];
  return {
    samples: samples.map((value) => Number(value.toFixed(1))),
    min: Number(sorted[0].toFixed(1)),
    median: Number(median.toFixed(1)),
    max: Number(sorted[sorted.length - 1].toFixed(1)),
  };
}

async function measureVariant(variant: VariantSpec): Promise<VariantResult> {
  const coldStarts: number[] = [];
  for (let iteration = 0; iteration < spec.coldStarts; iteration++) {
    const server = await start(variant);
    coldStarts.push(server.readyMs);
    await stop(server);
  }

  // One long-lived server for the memory and load phases.
  const server = await start(variant);
  await Bun.sleep(1000);
  const idleRssKb = rssKbTree(server.proc.pid);

  let peakRssKb = idleRssKb;
  let sampling = true;
  const sampler = (async () => {
    while (sampling) {
      const rss = rssKbTree(server.proc.pid);
      if (rss !== null && (peakRssKb === null || rss > peakRssKb)) peakRssKb = rss;
      await Bun.sleep(50);
    }
  })();

  const http = await httpLoad(server.port, spec.probePath, spec.httpRequests, spec.httpConcurrency);
  const ws = await wsLoad(server.port, spec.wsClients, spec.wsMessages);

  sampling = false;
  await sampler;
  const loadedRssKb = rssKbTree(server.proc.pid);
  await stop(server);

  return {
    name: variant.name,
    coldStartMs: stats(coldStarts),
    idleRssKb,
    loadedRssKb,
    peakRssKb,
    httpOps: http.ops,
    httpOpsPerSec: Number(http.opsPerSec.toFixed(0)),
    httpErrors: http.errors,
    wsMessagesReceived: ws.received,
    wsErrors: ws.errors,
  };
}

const results: VariantResult[] = [];
for (const variant of spec.variants) {
  process.stderr.write(`measuring ${variant.name}...\n`);
  results.push(await measureVariant(variant));
}

const payload = JSON.stringify({ host: { platform: process.platform, bun: Bun.version }, results }, null, 2);
if (outPath) writeFileSync(outPath, `${payload}\n`);
process.stdout.write(`${payload}\n`);
