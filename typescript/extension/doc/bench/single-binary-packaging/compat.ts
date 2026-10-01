#!/usr/bin/env bun
/**
 * Compatibility checklist for `bun build --compile`d Putnami servers.
 *
 * Every check starts a real server (compiled binary or `bun run` bundle) and
 * exercises one framework surface end to end, so a "pass" is an observed HTTP /
 * WebSocket response, never an inference from reading the code.
 *
 * Usage (see bench.sh compat):
 *   bun compat.ts --binary <path> --binary-disk <path> --binary-smol <path> \
 *     --binary-sourcemap <path> --bundle <path> --bun <bun> \
 *     --disk-cwd <dir> --embedded-cwd <dir> --assets-dir '/$bunfs/root/public' \
 *     --port 39880 --native-probe <path> [--out result.json]
 */
import { rmSync, writeFileSync } from 'node:fs';

type Status = 'pass' | 'fail' | 'partial';

interface CheckResult {
  check: string;
  status: Status;
  detail: string;
}

function arg(name: string, fallback?: string): string {
  const index = process.argv.indexOf(`--${name}`);
  if (index < 0 || index + 1 >= process.argv.length) {
    if (fallback !== undefined) return fallback;
    throw new Error(`missing --${name}`);
  }
  return process.argv[index + 1];
}

const options = {
  binary: arg('binary'),
  binaryDisk: arg('binary-disk'),
  binarySmol: arg('binary-smol'),
  binarySourcemap: arg('binary-sourcemap'),
  bundle: arg('bundle'),
  bun: arg('bun', 'bun'),
  diskCwd: arg('disk-cwd'),
  embeddedCwd: arg('embedded-cwd'),
  assetsDir: arg('assets-dir'),
  nativeProbe: arg('native-probe'),
  sample: arg('sample', ''),
  entry: arg('entry', '.gen/src/serve.bundled.ts'),
};

let nextPort = Number(arg('port', '45880'));

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
/** Thrown after a `record()` call so a check can bail without double-recording. */
class AlreadyRecorded extends Error {}

const results: CheckResult[] = [];
const record = (check: string, status: Status, detail: string) => {
  results.push({ check, status, detail });
  process.stderr.write(`${status === 'pass' ? 'PASS' : status === 'partial' ? 'PART' : 'FAIL'}  ${check}: ${detail}\n`);
};

interface Server {
  proc: Bun.Subprocess;
  port: number;
  output: () => Promise<string>;
}

async function waitForPort(port: number, path: string, timeoutMs = 20_000): Promise<boolean> {
  const deadline = performance.now() + timeoutMs;
  while (performance.now() < deadline) {
    try {
      const response = await fetch(`http://127.0.0.1:${port}${path}`);
      await response.arrayBuffer();
      return true;
    } catch {
      await Bun.sleep(5);
    }
  }
  return false;
}

async function startServer(cmd: string[], cwd: string, env: Record<string, string>, probe = '/index.html') {
  const port = takePort();
  const proc = Bun.spawn({
    cmd,
    cwd,
    env: {
      PATH: process.env['PATH'] ?? '/usr/bin:/bin',
      HOME: process.env['HOME'] ?? '/root',
      NODE_ENV: 'production',
      PORT: String(port),
      PWD: cwd,
      PUTNAMI_PROJECT_ROOT: cwd,
      ...env,
    },
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const collected: Promise<string> = (async () => {
    const [out, err] = await Promise.all([
      new Response(proc.stdout as ReadableStream).text(),
      new Response(proc.stderr as ReadableStream).text(),
    ]);
    return `${out}\n${err}`;
  })();
  const ready = await waitForPort(port, probe);
  return { proc, port, ready, output: () => collected } as Server & { ready: boolean };
}

async function stopServer(server: Server): Promise<string> {
  server.proc.kill();
  await Promise.race([server.proc.exited, Bun.sleep(3000).then(() => server.proc.kill(9))]);
  await server.proc.exited;
  return server.output();
}

const embeddedEnv = { PUTNAMI_ASSETS_DIR: options.assetsDir };

// ---------------------------------------------------------------------------
// 1 + 2. Static routes: the StaticPlugin route loader, from disk and from the
// executable's virtual FS.
// ---------------------------------------------------------------------------
async function checkStatic(label: string, cmd: string[], cwd: string, env: Record<string, string>) {
  const server = await startServer(cmd, cwd, env);
  try {
    if (!server.ready) {
      record(`static routes (${label})`, 'fail', 'server never became reachable');
      return;
    }
    const problems: string[] = [];
    for (const path of ['/', '/index', '/index.html']) {
      const response = await fetch(`http://127.0.0.1:${server.port}${path}`);
      await response.arrayBuffer();
      if (response.status !== 200) problems.push(`${path} -> ${response.status}`);
    }
    const gzip = await fetch(`http://127.0.0.1:${server.port}/index.html`, {
      headers: { 'Accept-Encoding': 'gzip' },
    });
    const gzipBytes = (await gzip.arrayBuffer()).byteLength;
    const etag = gzip.headers.get('etag');
    if (!etag) problems.push('no ETag (generateETag could not read the file)');
    const conditional = await fetch(`http://127.0.0.1:${server.port}/index.html`, {
      headers: { 'If-None-Match': etag ?? '""', 'Accept-Encoding': 'gzip' },
    });
    await conditional.arrayBuffer();
    if (conditional.status !== 304) problems.push(`conditional GET -> ${conditional.status}`);
    // HEAD is the only path that reports Last-Modified, and it comes from the
    // stat() taken when the route was registered — which is where a virtual-FS
    // file (mtime 0) differs from a file on disk.
    const head = await fetch(`http://127.0.0.1:${server.port}/index.html`, { method: 'HEAD' });
    await head.arrayBuffer();
    const lastModified = head.headers.get('last-modified') || '(empty)';
    const contentLength = head.headers.get('content-length') ?? '(none)';
    const detail = `GET / /index /index.html 200, conditional GET 304; gzip body ${gzipBytes}B; ETag ${etag ?? 'missing'}; HEAD Content-Length ${contentLength}, Last-Modified ${lastModified}`;
    record(`static routes (${label})`, problems.length === 0 ? 'pass' : 'fail', problems.join(', ') || detail);
  } finally {
    await stopServer(server);
  }
}

await checkStatic('compiled binary, --asset embedded', [options.binary], options.embeddedCwd, embeddedEnv);
await checkStatic('compiled binary, assets on disk', [options.binaryDisk], options.diskCwd, {});
await checkStatic('bun run bundle (baseline)', [options.bun, 'run', options.bundle], options.diskCwd, {});

// ---------------------------------------------------------------------------
// 3. --asset without PUTNAMI_ASSETS_DIR: documents that embedding alone is not
// enough — StaticPlugin still resolves <projectRoot>/.gen/public.
// ---------------------------------------------------------------------------
{
  const check = 'embedded assets without PUTNAMI_ASSETS_DIR';
  const server = await startServer([options.binary], options.embeddedCwd, {}, '/');
  try {
    if (!server.ready) {
      record(check, 'fail', 'server never became reachable');
    } else {
      const response = await fetch(`http://127.0.0.1:${server.port}/index.html`);
      await response.arrayBuffer();
      // Not a regression, a requirement: --asset embeds the bytes but does not
      // move StaticPlugin's <projectRoot>/.gen/public lookup. `partial` records
      // that the deployment must set assetsDir; `pass` would mean embedding alone
      // is sufficient.
      record(
        check,
        response.status === 200 ? 'pass' : 'partial',
        `GET /index.html -> ${response.status}; embedding alone does not redirect StaticPlugin's file reads, the deployment must set PUTNAMI_ASSETS_DIR`,
      );
    }
  } catch (error) {
    record(check, 'fail', `probe error: ${(error as Error).message}`);
  } finally {
    await stopServer(server);
  }
}

// ---------------------------------------------------------------------------
// 4. WebSocket: join + broadcast round trip through the typed API layer.
// ---------------------------------------------------------------------------
{
  const check = 'WebSocket (/chat, typed Stream endpoint)';
  const server = await startServer([options.binary], options.embeddedCwd, embeddedEnv);
  try {
    if (!server.ready) {
      record(check, 'fail', 'server never became reachable');
    } else {
      const frames: Record<string, unknown>[] = [];
      const socket = new WebSocket(`ws://127.0.0.1:${server.port}/chat`);
      const done = new Promise<string>((resolve) => {
        const timer = setTimeout(() => resolve('timeout waiting for broadcast'), 5000);
        socket.onerror = () => {
          clearTimeout(timer);
          resolve('socket error');
        };
        socket.onmessage = (event) => {
          frames.push(JSON.parse(String(event.data)));
          if (frames.some((frame) => frame['type'] === 'message')) {
            clearTimeout(timer);
            resolve('');
          }
        };
        socket.onopen = () => {
          socket.send(JSON.stringify({ type: 'join', username: 'compat' }));
          socket.send(JSON.stringify({ type: 'message', text: 'hello from a compiled binary' }));
        };
      });
      const failure = await done;
      socket.close();
      const kinds = frames.map((frame) => String(frame['type'])).join(',');
      record(check, failure ? 'fail' : 'pass', failure || `received frames: ${kinds}`);
    }
  } catch (error) {
    record(check, 'fail', `probe error: ${(error as Error).message}`);
  } finally {
    await stopServer(server);
  }
}

// ---------------------------------------------------------------------------
// 5. Server-Sent Events: the other real-time surface of the sample.
// ---------------------------------------------------------------------------
{
  const check = 'SSE (/notifications, typed Stream endpoint)';
  const server = await startServer([options.binary], options.embeddedCwd, embeddedEnv);
  try {
    if (!server.ready) {
      record(check, 'fail', 'server never became reachable');
    } else {
      const controller = new AbortController();
      const response = await fetch(`http://127.0.0.1:${server.port}/notifications`, {
        headers: { Accept: 'text/event-stream' },
        signal: controller.signal,
      });
      let first = '';
      if (response.ok && response.body) {
        const reader = response.body.getReader();
        const chunk = await Promise.race([reader.read(), Bun.sleep(5000).then(() => undefined)]);
        if (chunk && !chunk.done) first = new TextDecoder().decode(chunk.value).trim().split('\n')[0];
      }
      controller.abort();
      record(
        check,
        response.ok && first.startsWith('data:') ? 'pass' : 'fail',
        `status ${response.status} ${response.headers.get('content-type') ?? ''}; first frame ${first || '(none)'}`,
      );
    }
  } catch (error) {
    record(check, 'fail', `probe error: ${(error as Error).message}`);
  } finally {
    await stopServer(server);
  }
}

// ---------------------------------------------------------------------------
// 6. --smol, baked in with --compile-exec-argv (a compiled binary has no CLI
// surface to pass runtime flags on).
// ---------------------------------------------------------------------------
{
  const check = '--smol via --compile-exec-argv';
  const server = await startServer([options.binarySmol], options.embeddedCwd, embeddedEnv);
  try {
    if (!server.ready) {
      record(check, 'fail', 'server never became reachable');
    } else {
      const response = await fetch(`http://127.0.0.1:${server.port}/index.html`);
      await response.arrayBuffer();
      record(check, response.status === 200 ? 'pass' : 'fail', `GET /index.html -> ${response.status}`);
    }
  } catch (error) {
    record(check, 'fail', `probe error: ${(error as Error).message}`);
  } finally {
    await stopServer(server);
  }
}

// ---------------------------------------------------------------------------
// 7. Bun.serve `routes` + raw upgrade, from a compiled standalone probe.
// ---------------------------------------------------------------------------
{
  const probeBinary = `${options.embeddedCwd}/native-routes-probe.bin`;
  const build = Bun.spawnSync({
    cmd: [options.bun, 'build', '--compile', options.nativeProbe, '--outfile', probeBinary],
    stdout: 'pipe',
    stderr: 'pipe',
  });
  if (!build.success) {
    record('Bun.serve routes in a compiled binary', 'fail', `compile failed: ${build.stderr.toString().slice(0, 300)}`);
  } else {
    const check = 'Bun.serve routes in a compiled binary';
    const server = await startServer([probeBinary], options.embeddedCwd, {}, '/native');
    try {
      if (!server.ready) {
        record(check, 'fail', 'probe server never became reachable');
        throw new AlreadyRecorded();
      }
      const problems: string[] = [];
      const base = `http://127.0.0.1:${server.port}`;
      const exact = await (await fetch(`${base}/native`)).text();
      if (exact !== 'native-ok') problems.push(`/native -> ${exact}`);
      const param = await (await fetch(`${base}/native/42`)).text();
      if (param !== 'native-param:42') problems.push(`/native/42 -> ${param}`);
      const fallthrough = await (await fetch(`${base}/elsewhere`)).text();
      if (fallthrough !== 'fallthrough-ok') problems.push(`/elsewhere -> ${fallthrough}`);
      const echo = await new Promise<string>((resolve) => {
        const socket = new WebSocket(`ws://127.0.0.1:${server.port}/ws`);
        const timer = setTimeout(() => resolve('timeout'), 5000);
        socket.onmessage = (event) => {
          clearTimeout(timer);
          resolve(String(event.data));
          socket.close();
        };
        socket.onerror = () => {
          clearTimeout(timer);
          resolve('error');
        };
        socket.onopen = () => socket.send('ping');
      });
      if (echo !== 'echo:ping') problems.push(`ws upgrade -> ${echo}`);
      record(
        check,
        problems.length === 0 ? 'pass' : 'fail',
        problems.join(', ') || 'exact route, :param route, fetch fallthrough and server.upgrade() all answered',
      );
    } catch (error) {
      if (!(error instanceof AlreadyRecorded)) record(check, 'fail', `probe error: ${(error as Error).message}`);
    } finally {
      await stopServer(server);
    }
  }
}

// ---------------------------------------------------------------------------
// 8. Startup-failure stack traces through bootstrapServe. A busy port makes
// Bun.serve throw inside app.start(), which is the path bootstrapServe guards.
// ---------------------------------------------------------------------------
async function checkStartupTrace(label: string, cmd: string[], cwd: string, env: Record<string, string>) {
  const check = `startup-failure trace (${label})`;
  const holder = await startServer(cmd, cwd, env);
  try {
    if (!holder.ready) {
      // Without a live holder the "collision" would bind the port and serve
      // forever, so there is no EADDRINUSE to trace.
      record(check, 'fail', 'port-holder server never became reachable');
      return;
    }
    const collision = Bun.spawn({
      cmd,
      cwd,
      env: {
        PATH: process.env['PATH'] ?? '/usr/bin:/bin',
        HOME: process.env['HOME'] ?? '/root',
        NODE_ENV: 'production',
        PORT: String(holder.port),
        PWD: cwd,
        PUTNAMI_PROJECT_ROOT: cwd,
        ...env,
      },
      stdout: 'pipe',
      stderr: 'pipe',
    });
    // Draining to EOF only resolves when the collision exits, which it does on
    // EADDRINUSE — but if it somehow binds (SO_REUSEPORT, a TOCTOU on the
    // holder's port), it would serve forever: bound the wait and kill it.
    const drained = Promise.all([
      new Response(collision.stdout as ReadableStream).text(),
      new Response(collision.stderr as ReadableStream).text(),
    ]);
    const outcome = await Promise.race([drained, Bun.sleep(15_000).then(() => 'timeout' as const)]);
    if (outcome === 'timeout') {
      collision.kill(9);
      await collision.exited;
      record(check, 'fail', 'collision process kept running — the port was not exclusively held, no failure to trace');
      return;
    }
    await collision.exited;
    const [out, err] = outcome;
    const text = `${out}\n${err}`;
    const guarded = text.includes('startup failed');
    // Frames either carry a source extension (.ts/.js) or sit inside the
    // executable's virtual FS (/$bunfs/... with no extension) — match both, so
    // the compiled-binary row still shows where its frames point.
    const frames = [...text.matchAll(/at [^\n]*?((?:[\w./-]+\.(?:ts|js)|\/\$bunfs\/[\w.$/-]+)):(\d+)/g)].map(
      (match) => match[1],
    );
    const original = frames.filter((frame) => /\.ts$/.test(frame));
    const shortFrames = frames.slice(0, 3).map((frame) => frame.split('/').slice(-2).join('/'));
    record(
      check,
      guarded ? (original.length > 0 ? 'pass' : 'partial') : 'fail',
      `exit ${collision.exitCode}; bootstrapServe guard ${guarded ? 'logged one line' : 'did NOT log'}; frames: ${shortFrames.join(' ') || '(none)'}`,
    );
  } finally {
    await stopServer(holder);
  }
}

if (options.sample) {
  // The real baseline: `putnami serve` runs the generated entrypoint from
  // source, with the workspace on disk.
  await checkStartupTrace('bun run from source (baseline)', [options.bun, 'run', options.entry], options.sample, {});
}
await checkStartupTrace('bun run bundle', [options.bun, 'run', options.bundle], options.diskCwd, {});
await checkStartupTrace('compiled binary', [options.binary], options.embeddedCwd, embeddedEnv);
await checkStartupTrace(
  'compiled binary + bytecode + --sourcemap=inline',
  [options.binarySourcemap],
  options.embeddedCwd,
  embeddedEnv,
);

// ---------------------------------------------------------------------------
// 9. --bytecode without --format=esm, against the generated bundled entrypoint
// (which ends in a top-level await).
// ---------------------------------------------------------------------------
if (options.sample) {
  // Never /dev/null: when bun proceeds far enough to write, it replaces the
  // outfile with a regular file — handing it the device node destroys it the
  // day this compile starts succeeding. A scratch path costs nothing.
  const probeOut = `${options.embeddedCwd}/bytecode-cjs-probe.bin`;
  const build = Bun.spawnSync({
    cmd: [options.bun, 'build', '--compile', '--bytecode', options.entry, '--outfile', probeOut],
    cwd: options.sample,
    stdout: 'pipe',
    stderr: 'pipe',
  });
  rmSync(probeOut, { force: true });
  const message = build.stderr.toString().replace(/\s+/g, ' ').trim().slice(0, 200);
  // The interesting outcome is the *specific* top-level-await rejection; any
  // other failure (disk, permissions) must not read as "still rejected".
  const expectedRejection = message.includes('"await" can only be used inside an "async" function');
  record(
    '--bytecode without --format=esm',
    build.success ? 'pass' : 'fail',
    build.success ? 'compiled' : expectedRejection ? `rejected: ${message}` : `failed for another reason: ${message}`,
  );
}

const outIndex = process.argv.indexOf('--out');
const payload = JSON.stringify({ results }, null, 2);
if (outIndex > 0) writeFileSync(process.argv[outIndex + 1], `${payload}\n`);
process.stdout.write(`${payload}\n`);
