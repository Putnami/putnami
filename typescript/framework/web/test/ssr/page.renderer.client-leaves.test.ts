import { afterEach, describe, expect } from 'bun:test';
import net from 'node:net';
import { application, HttpPlugin } from '@putnami/application';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import React, { Suspense, use } from 'react';
import RootHtml from '../../src/ssr/default.html';
import { pageRenderer } from '../../src/ssr/page.renderer';

type Mode = 'suspense' | 'flood' | 'crawler';

/** A page whose Suspense boundaries resolve one by one over about 600 ms. */
function pageFor(mode: Mode): React.ReactElement {
  const delays = Array.from({ length: 40 }, (_, i) => (i + 1) * 15);
  function Item({ late, index }: { late: Promise<string>; index: number }): React.ReactElement {
    return React.createElement('p', null, `${use(late)} ${index} ${'x'.repeat(mode === 'flood' ? 20_000 : 10)}`);
  }
  function Page(): React.ReactElement {
    const lates = React.useMemo(
      () => delays.map((delay) => new Promise<string>((resolve) => setTimeout(() => resolve('late'), delay))),
      [],
    );
    return React.createElement(
      'main',
      null,
      React.createElement('h1', null, `Shell ${'y'.repeat(2000)}`),
      ...lates.map((late, index) =>
        React.createElement(
          Suspense,
          { key: index, fallback: React.createElement('i', null, '…') },
          React.createElement(Item, { late, index }),
        ),
      ),
    );
  }
  return React.createElement(Page);
}

/**
 * Serves the page, lets a client read part of it and drop its socket, and
 * returns what the server logged at ERROR and the rejections nobody handled.
 */
async function leaveMidStream(mode: Mode): Promise<{ errors: unknown[]; rejections: unknown[]; received: string }> {
  const memory = new MemoryLogger();
  setRootLogger(memory);
  const rejections: unknown[] = [];
  const onRejection = (reason: unknown) => rejections.push(reason);
  process.on('unhandledRejection', onRejection);
  const element = pageFor(mode);
  const plugin = new HttpPlugin({ port: 0, secure: false });
  plugin.get(
    '/',
    pageRenderer(
      () => [{ path: '/', element } as never],
      () => RootHtml,
      () => 'hydrate.js',
      5000,
      undefined,
      undefined,
      undefined,
      () => mode === 'crawler',
    ),
  );
  const app = application().use(plugin);
  await app.start();
  let received = '';
  try {
    const port = plugin.getServer()?.port as number;
    await new Promise<void>((resolve) => {
      const socket = net.connect(port, '127.0.0.1', () =>
        socket.write('GET / HTTP/1.1\r\nHost: localhost\r\nAccept: text/html\r\n\r\n'),
      );
      socket.on('error', () => undefined);
      socket.on('close', () => resolve());
      socket.on('data', (data) => {
        received += data.toString();
        if (mode !== 'crawler' && received.includes('id="root"') && received.length > 3000) socket.destroy();
      });
      // A crawler waits for every boundary before its first byte: leave while it waits.
      if (mode === 'crawler') setTimeout(() => socket.destroy(), 100);
    });
    // Let the render and the stream observe the closed connection.
    await new Promise((resolve) => setTimeout(resolve, 1000));
  } finally {
    await app.stop();
    process.off('unhandledRejection', onRejection);
    resetDefaultLogger();
  }
  return {
    errors: memory.entries.filter((entry) => entry.level === 'error'),
    rejections,
    received,
  };
}

describe('page.renderer served stream', () => {
  afterEach(() => resetDefaultLogger());

  for (const mode of ['suspense', 'flood', 'crawler'] as const) {
    specTest(
      `logs nothing when the client leaves mid-stream (${mode})`,
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'ssr-streaming',
        check: 'a-client-that-leaves-mid-stream-logs-nothing',
      },
      async () => {
        const { errors, rejections, received } = await leaveMidStream(mode);

        // The client did leave before the document ended.
        expect(received).not.toContain('</html>');
        expect(errors).toEqual([]);
        expect(rejections).toEqual([]);
      },
      10_000,
    );
  }
});
