import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { readLocalLogLevel, resolveLogLevel, watchLogLevel } from '../../src/logger/log-level';
import { type LogRecordOutcome, observeLogRecords } from '../../src/logger/log-records';
import { Logger } from '../../src/logger/logger';
import { currentLogLevel, resetLoggerConfig, setLogLevel } from '../../src/logger/logger.config';
import type { LogLevel } from '../../src/logger/logger.type';
import { MemoryLogger, MemorySink } from '../../src/logger/memory.logger';
import { specTest } from '../../src/spectest';

const feature = 'typescript/structured-logging';

describe('live log level', () => {
  let savedLogLevel: string | undefined;
  let savedConfigData: string | undefined;

  beforeEach(() => {
    savedLogLevel = process.env['LOG_LEVEL'];
    savedConfigData = process.env['CONFIG_DATA'];
    delete process.env['LOG_LEVEL'];
    delete process.env['CONFIG_DATA'];
    resetLoggerConfig();
  });

  afterEach(() => {
    if (savedLogLevel === undefined) delete process.env['LOG_LEVEL'];
    else process.env['LOG_LEVEL'] = savedLogLevel;
    if (savedConfigData === undefined) delete process.env['CONFIG_DATA'];
    else process.env['CONFIG_DATA'] = savedConfigData;
    resetLoggerConfig();
  });

  specTest(
    'logger.level wins, then LOG_LEVEL, then info',
    { feature, requirement: 'live-level', check: 'the-level-reads-the-config-key' },
    () => {
      process.env['LOG_LEVEL'] = 'warn';
      expect(resolveLogLevel('debug')).toBe('debug');
      expect(resolveLogLevel('')).toBe('warn');
      expect(resolveLogLevel('verbose')).toBe('info');
      // Parsed like Go's ParseLevel: case is ignored, warning means warn.
      expect(resolveLogLevel('WARN')).toBe('warn');
      expect(resolveLogLevel('Warning')).toBe('warn');
      delete process.env['LOG_LEVEL'];
      expect(resolveLogLevel('')).toBe('info');

      process.env['CONFIG_DATA'] = 'logger:\n  level: error\n';
      expect(readLocalLogLevel()).toBe('error');
      delete process.env['CONFIG_DATA'];
      expect(readLocalLogLevel()).toBe('');
    },
  );

  specTest(
    'setLogLevel applies to the next record of every logger',
    { feature, requirement: 'live-level', check: 'set-log-level-applies-to-the-next-record' },
    () => {
      const sink = new MemorySink();
      const logger = new Logger([sink], 'live').with('k', 'v');

      setLogLevel('info');
      logger.debug('hidden');
      expect(sink.entries).toHaveLength(0);

      setLogLevel('debug');
      logger.debug('shown');
      expect(sink.entries.map((e) => e.message)).toEqual(['shown']);
      expect(currentLogLevel()).toBe('debug');
    },
  );

  specTest(
    'the watcher applies a changed logger.level and keeps a level set since start',
    { feature, requirement: 'live-level', check: 'the-watcher-applies-a-changed-level' },
    async () => {
      let value = 'info';
      let reads = 0;
      const read = () => {
        reads++;
        return value;
      };
      const waitReads = async (n: number) => {
        const target = reads + n;
        const deadline = Date.now() + 5000;
        while (reads < target) {
          if (Date.now() > deadline) throw new Error('the watcher stopped reading');
          await Bun.sleep(5);
        }
      };

      setLogLevel('warn');
      const stop = watchLogLevel(0.01, read);
      expect(reads).toBe(1); // the baseline read happens before watchLogLevel returns
      await waitReads(3);
      expect(currentLogLevel()).toBe('warn');

      value = 'debug';
      await waitReads(2);
      expect(currentLogLevel()).toBe('debug');

      stop();
      const after = reads;
      value = 'error';
      await Bun.sleep(50);
      expect(reads).toBe(after);
      expect(currentLogLevel()).toBe('debug');

      // A zero interval reads nothing.
      watchLogLevel(0, read)();
      expect(reads).toBe(after);
    },
  );

  specTest(
    'an error record is emitted whatever the level',
    { feature, requirement: 'errors-always-emitted', check: 'an-error-record-ignores-the-level' },
    () => {
      const sink = new MemorySink();
      const logger = new Logger([sink], 'floor');
      for (const level of ['debug', 'info', 'warn', 'error'] as LogLevel[]) {
        setLogLevel(level);
        logger.error('kept');
      }
      setLogLevel('error');
      logger.warn('hidden');
      expect(sink.entries.map((e) => e.message)).toEqual(['kept', 'kept', 'kept', 'kept']);
    },
  );

  specTest(
    'every record is counted by level and outcome',
    { feature, requirement: 'records-counted', check: 'records-are-counted-per-level-and-outcome' },
    () => {
      const counts = new Map<string, number>();
      const remove = observeLogRecords((level: LogLevel, outcome: LogRecordOutcome) => {
        const key = `${level}/${outcome}`;
        counts.set(key, (counts.get(key) ?? 0) + 1);
      });

      setLogLevel('info');
      const logger = new Logger([new MemorySink()], 'count');
      logger.debug('d');
      logger.info('i');
      logger.info('i');
      logger.warn('w');
      logger.error('e');
      // A logger that skips the level check counts its records as emitted.
      new MemoryLogger('mem').debug('d');

      remove();
      logger.info('after removal');

      expect(Object.fromEntries(counts)).toEqual({
        'debug/dropped': 1,
        'info/emitted': 2,
        'warn/emitted': 1,
        'error/emitted': 1,
        'debug/emitted': 1,
      });
    },
  );
});
