/**
 * A process that serves one request with the telemetry its environment gives
 * it, then stops, so its stop pushes to the collector the environment names.
 * The process-instance-identity tests start it twice from one environment.
 */
import { application } from '../../../src/application';
import { http } from '../../../src/http/http.plugin';

const server = http({ port: 0 });
server.get('/ping', () => ({ ok: true }));
const app = application().use(server);
await app.start();
await fetch(`http://localhost:${server.getServer()?.port}/ping`);
await app.stop();
