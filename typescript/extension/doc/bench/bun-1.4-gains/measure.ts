// Measurement probes for the Bun 1.4.1 to 1.4.5 gains. Run each mode with the Bun
// under test; bench.sh drives them and stores one JSON line per run.
//
//   sql <bun|postgresjs> <queries>     CPU and wall per point SELECT, one connection
//   idle <seconds> <cmd...>            CPU a workload burns while idle after ready
//   startup <runs> <cold|warm> <bin...> spawn-to-ready time of compiled executables
//   rss <cwd> <cmd...>                 peak RSS and wall time of a command (bun test)
//   stats <file.jsonl> <keys> <metric> median, spread and range of recorded runs
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const [mode, ...rest] = process.argv.slice(2);

// The ready line every Putnami workload logs once it serves.
const READY = '"target":"workload"';
const CLOCK_TICK_MS = 10; // USER_HZ = 100 on Linux

let nextPort = Number(process.env.BENCH_BASE_PORT ?? 45180);

function cpuMs(pid: number): number {
  const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
  const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
  return (Number(fields[11]) + Number(fields[12])) * CLOCK_TICK_MS;
}

async function spawnReady(cmd: string[], cwd?: string) {
  const proc = Bun.spawn(cmd, {
    cwd,
    stdout: 'pipe',
    stderr: 'ignore',
    env: { ...process.env, PORT: String(nextPort++) },
  });
  const reader = proc.stdout.getReader();
  const decoder = new TextDecoder();
  let seen = '';
  while (!seen.includes(READY)) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`${cmd[0]} exited before ready: ${seen.slice(-400)}`);
    seen += decoder.decode(value);
  }
  // Keep draining so the workload never blocks on a full pipe.
  void (async () => {
    while (!(await reader.read()).done) {}
  })();
  return proc;
}

async function sql(driver: string, queries: number) {
  const url = process.env.DATABASE_URL ?? 'postgres://postgres:postgres@127.0.0.1:5432/bench';
  let query: (id: number) => Promise<unknown>;
  let close: () => Promise<void>;
  if (driver === 'bun') {
    const client = new Bun.SQL(url, { max: 1 });
    query = (id) => client`SELECT id, name, score FROM point WHERE id = ${id}`;
    close = () => client.close();
  } else {
    // postgres.js, the driver @putnami/database uses; BENCH_POSTGRES_JS names its entry file.
    const postgres = (await import(process.env.BENCH_POSTGRES_JS as string)).default;
    const client = postgres(url, { max: 1 });
    query = (id) => client`SELECT id, name, score FROM point WHERE id = ${id}`;
    close = () => client.end();
  }
  for (let i = 0; i < 2000; i++) await query((i % 1000) + 1);
  const cpu0 = process.cpuUsage();
  const t0 = performance.now();
  for (let i = 0; i < queries; i++) await query((i % 1000) + 1);
  const wallUs = (performance.now() - t0) * 1000;
  const cpu = process.cpuUsage(cpu0);
  await close();
  return { bun: Bun.version, driver, cpuUsPerQuery: (cpu.user + cpu.system) / queries, wallUsPerQuery: wallUs / queries };
}

async function idle(seconds: number, cmd: string[]) {
  const proc = await spawnReady(cmd);
  await Bun.sleep(2000); // let start-up work settle
  const before = cpuMs(proc.pid);
  await Bun.sleep(seconds * 1000);
  const after = cpuMs(proc.pid);
  proc.kill();
  await proc.exited;
  return { cmd: cmd[0], seconds, idleCpuMs: after - before };
}

async function startup(runs: number, cache: string, bins: string[]) {
  const rows: { bin: string; cache: string; ms: number }[] = [];
  for (let i = 0; i < runs; i++) {
    for (const bin of bins) {
      if (cache === 'cold') spawnSync('sh', ['-c', 'sync; echo 3 > /proc/sys/vm/drop_caches']);
      const t0 = performance.now();
      const proc = await spawnReady([bin]);
      rows.push({ bin, cache, ms: performance.now() - t0 });
      proc.kill();
      await proc.exited;
    }
  }
  return rows;
}

async function rss(cwd: string, cmd: string[]) {
  const t0 = performance.now();
  // PWD follows cwd: some suites resolve the project from it, as a shell would set it.
  const proc = Bun.spawn(cmd, { cwd, env: { ...process.env, PWD: resolve(cwd) }, stdout: 'pipe', stderr: 'pipe' });
  const [, stderr] = await Promise.all([new Response(proc.stdout).text(), new Response(proc.stderr).text()]);
  await proc.exited;
  const usage = proc.resourceUsage();
  const ran = stderr.match(/(\d+) pass[\s\S]*?(\d+) fail/);
  return {
    exit: proc.exitCode,
    maxRssMB: Math.round((usage?.maxRSS ?? 0) / 1024 / 1024),
    wallS: (performance.now() - t0) / 1000,
    pass: Number(ran?.[1] ?? 0),
    fail: Number(ran?.[2] ?? 0),
  };
}

function stats(file: string, keys: string, metric: string) {
  const groups = new Map<string, number[]>();
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    if (!line.startsWith('{')) continue;
    const row = JSON.parse(line);
    const key = keys
      .split(',')
      .map((k) => row[k])
      .join(' ');
    groups.set(key, [...(groups.get(key) ?? []), row[metric]]);
  }
  for (const [key, values] of groups) {
    const s = values.toSorted((a, b) => a - b);
    const n = s.length;
    const median = n % 2 ? s[(n - 1) / 2] : (s[n / 2 - 1] + s[n / 2]) / 2;
    const mean = s.reduce((a, b) => a + b, 0) / n;
    const sd = Math.sqrt(s.reduce((a, b) => a + (b - mean) ** 2, 0) / Math.max(n - 1, 1));
    const f = (x: number) => Number(x.toPrecision(3)).toString();
    console.log(`${key.padEnd(36)} n=${n} median=${f(median)} sd=${f(sd)} min=${f(s[0])} max=${f(s[n - 1])}`);
  }
}

switch (mode) {
  case 'sql':
    console.log(JSON.stringify(await sql(rest[0], Number(rest[1]))));
    break;
  case 'idle':
    console.log(JSON.stringify(await idle(Number(rest[0]), rest.slice(1))));
    break;
  case 'startup':
    for (const row of await startup(Number(rest[0]), rest[1], rest.slice(2))) console.log(JSON.stringify(row));
    break;
  case 'rss':
    console.log(JSON.stringify(await rss(rest[0], rest.slice(1))));
    break;
  case 'stats':
    stats(rest[0], rest[1], rest[2]);
    break;
  default:
    throw new Error(`unknown mode ${mode}`);
}
