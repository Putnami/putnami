// In-process cost of building the health route's Response, per variant (us per Response).
const body = JSON.stringify({ status: 'ok' });
const entries: [string, string][] = [
  ['Content-Type', 'application/json'],
  ['Content-Security-Policy', "frame-ancestors 'none'"],
  ['X-Content-Type-Options', 'nosniff'],
];
const template = new Headers(entries);
const plain = Object.fromEntries(entries);
const variants: Record<string, () => Response> = {
  'Headers template + status + statusText (Putnami)': () => new Response(body, { headers: template, status: 200, statusText: 'OK' }),
  'Headers template + status': () => new Response(body, { headers: template, status: 200 }),
  'plain object headers + status (Elysia/Bun subjects)': () => new Response(body, { headers: plain, status: 200 }),
  'plain object + status + statusText': () => new Response(body, { headers: plain, status: 200, statusText: 'OK' }),
  'Response.json(value, {headers: plain})': () => Response.json({ status: 'ok' }, { headers: plain }),
};
const N = Number(process.argv[2] ?? 2_000_000);
let sink = 0;
for (let round = 0; round < 3; round++) {
  for (const [name, make] of Object.entries(variants)) {
    for (let i = 0; i < 50_000; i++) sink += make().status; // warm
    const c0 = process.cpuUsage();
    for (let i = 0; i < N; i++) sink += make().status;
    const c = process.cpuUsage(c0);
    console.log(`round ${round + 1}  ${((c.user + c.system) / N).toFixed(3)} us  ${name}`);
  }
}
if (sink === 0) console.log(sink);
