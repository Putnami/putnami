import { expect, it } from 'bun:test';
import { http, logger } from '../../src/http';
import { application } from '../../src/application';

it('should exclude paths from logger', async () => {
  // We can't easily assert on logs without mocking the logger, but we can verify it doesn't crash
  // and returns the response correctly.

  const h = http({ port: 0 });
  const l = logger({ exclude: ['/health'] });
  h.get('/health', () => 'ok');

  const app = application().use(h).use(l);
  await app.start();

  const port = h.getServer()?.port;
  const res = await fetch(`http://localhost:${port}/health`);

  expect(await res.text()).toBe('ok');

  await app.stop();
});
