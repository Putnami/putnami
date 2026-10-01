import { MemoryServer } from './server/memory-server';

const rawPort = Number(process.env['EVENTS_PORT'] || process.env['PORT']);
const port = rawPort >= 1 && rawPort <= 65_535 ? rawPort : 4222;
const server = new MemoryServer({ port });

await server.start();
