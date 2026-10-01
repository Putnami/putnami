#!/usr/bin/env bun
/**
 * Standalone probe for the two Bun.serve features the framework relies on but
 * that no sample exercises through a compiled binary:
 *
 * - `routes` — the native route table `HttpPlugin.native()` builds
 *   (http.plugin.ts `buildNativeRoutes`), including a parameterised route;
 * - `server.upgrade()` — the raw WebSocket upgrade under the typed API layer.
 *
 * Deliberately dependency-free so it can be compiled from anywhere in the
 * workspace (the doc tree has no node_modules of its own).
 */
const port = Number(process.env['PORT'] ?? 3000);

Bun.serve({
  port,
  hostname: '127.0.0.1',
  routes: {
    '/native': new Response('native-ok'),
    '/native/:id': (request: Bun.BunRequest<'/native/:id'>) => new Response(`native-param:${request.params.id}`),
  },
  fetch(request, server) {
    if (new URL(request.url).pathname === '/ws' && server.upgrade(request)) return undefined;
    return new Response('fallthrough-ok');
  },
  websocket: {
    message(ws, message) {
      ws.send(`echo:${message}`);
    },
  },
});
