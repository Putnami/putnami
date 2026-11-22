import { afterEach, describe, expect, it } from 'bun:test';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { type Application, application, type Plugin } from '../../src/application';
import { registerBundledStartGuard, reportPendingBundledStartGuard } from '../../src/bundled';

let app: Application | undefined;

afterEach(async () => {
  await app?.stop();
  app = undefined;
  resetDefaultLogger();
});

describe('bundled start guard', () => {
  it('runs once, on the first application that starts, before any plugin starts', async () => {
    setRootLogger(new MemoryLogger());
    const events: string[] = [];
    const plugin: Plugin = {
      start: async () => {
        events.push('plugin start');
      },
    };
    registerBundledStartGuard((started) => {
      events.push(started === app ? 'guard on the started app' : 'guard on another app');
    });

    app = application().use(plugin);
    await app.start();
    await app.stop();
    await app.start();

    expect(events).toEqual(['guard on the started app', 'plugin start', 'plugin start']);
  });

  it('fails the start when the guard throws, before any plugin starts', async () => {
    setRootLogger(new MemoryLogger());
    let pluginStarted = false;
    registerBundledStartGuard(() => {
      throw new Error('capability manifest config definitions were not activated: "db"');
    });

    app = application().use({
      start: async () => {
        pluginStarted = true;
      },
    });

    await expect(app.start()).rejects.toThrow('were not activated');
    expect(pluginStarted).toBe(false);
  });

  it('warns while no application took the guard, and keeps it for a later start', async () => {
    const memory = new MemoryLogger();
    setRootLogger(memory);
    let guardRuns = 0;
    registerBundledStartGuard(() => {
      guardRuns++;
    });

    reportPendingBundledStartGuard();
    app = application();
    await app.start();

    expect(memory.entries.filter((entry) => entry.message.includes('no Application had started'))).toHaveLength(1);
    expect(guardRuns).toBe(1);
  });

  it('stays silent once an application took the guard', async () => {
    const memory = new MemoryLogger();
    setRootLogger(memory);
    registerBundledStartGuard(() => {});
    app = application();
    await app.start();

    reportPendingBundledStartGuard();

    expect(memory.entries.some((entry) => entry.message.includes('no Application had started'))).toBe(false);
  });
});
