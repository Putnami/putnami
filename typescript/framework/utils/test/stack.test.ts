import { describe, expect, it } from 'bun:test';
import { getCallerInfo, getExternalCaller, parseStackTrace, type StackElement } from '../src';
import { isPutnamiInternalPath } from '../src/server/stack.utils';

/** Formats every stack as `stack` while `run` executes. */
function withStack<T>(stack: string, run: () => T): T {
  const stackError = Error as ErrorConstructor & {
    prepareStackTrace?: (_error: Error, _stack: unknown[]) => string;
  };
  const previous = stackError.prepareStackTrace;
  try {
    stackError.prepareStackTrace = () => stack;
    return run();
  } finally {
    stackError.prepareStackTrace = previous;
  }
}

/** A stack as Bun prints it on Windows: a drive and backslashes in every file. */
const WINDOWS_STACK = [
  'Error',
  '    at getExternalCaller (C:\\w\\src\\typescript\\framework\\utils\\src\\server\\stack.utils.ts:70:19)',
  '    at api (C:\\w\\src\\typescript\\framework\\application\\src\\api\\api.plugin.ts:843:20)',
  '    at C:\\Users\\dev\\app\\node_modules\\@putnami\\application\\src\\index.ts:1:1',
  '    at <anonymous> (C:\\Users\\dev\\app\\api\\src\\index.ts:3:17)',
].join('\n');

describe('stack.utils', () => {
  describe('getCallerInfo / getCaller', () => {
    // Helper to test caller info at various depths
    function level1(): StackElement | undefined {
      return getCallerInfo();
    }
    function level2(): StackElement | undefined {
      return level1();
    }

    it('returns a StackElement or undefined', () => {
      // The function should not throw
      const caller = getCallerInfo();
      expect(caller === undefined || typeof caller === 'object').toBe(true);
    });

    it('returns caller info when called from nested functions', () => {
      const caller = level2();
      // In nested calls, we should get stack info
      if (caller) {
        expect(caller.filePath).toBeDefined();
        expect(typeof caller.lineNumber).toBe('number');
      }
    });
  });

  describe('getExternalCaller', () => {
    it('returns undefined when called from within @putnami packages', () => {
      // When called from within the test suite (which is inside packages/utils/),
      // getExternalCaller correctly returns undefined since all frames are internal
      const caller = getExternalCaller();
      expect(caller).toBeUndefined();
    });

    it('would return a valid stack element when called from external code', () => {
      // This test documents the expected behavior: when called from user code
      // (outside @putnami packages), it returns the caller's info.
      // We can't easily test this from within the package itself.
      // The actual validation happens in integration testing.
      const caller = getExternalCaller();
      // In our test environment (inside @putnami), this is undefined
      expect(caller === undefined || typeof caller === 'object').toBe(true);
    });

    it('makes external provenance project-relative and drops host paths outside the project', () => {
      const stackError = Error as ErrorConstructor & {
        prepareStackTrace?: (_error: Error, _stack: unknown[]) => string;
      };
      const previous = stackError.prepareStackTrace;
      try {
        stackError.prepareStackTrace = () => 'Error\n    at declareFeature (/workspace/app/src/feature.ts:12:4)';
        expect(getExternalCaller('/workspace/app')).toMatchObject({
          functionName: 'declareFeature',
          filePath: 'src/feature.ts',
          lineNumber: 12,
        });

        stackError.prepareStackTrace = () => 'Error\n    at declareFeature (/Users/builder/private/feature.ts:12:4)';
        expect(getExternalCaller('/workspace/app')).toBeUndefined();
      } finally {
        stackError.prepareStackTrace = previous;
      }
    });

    it('degrades to no provenance when stack formatting fails', () => {
      const stackError = Error as ErrorConstructor & {
        prepareStackTrace?: (_error: Error, _stack: unknown[]) => string;
      };
      const previous = stackError.prepareStackTrace;
      try {
        stackError.prepareStackTrace = () => {
          throw new Error('stack formatter failed');
        };
        expect(getExternalCaller('/workspace/app')).toBeUndefined();
      } finally {
        stackError.prepareStackTrace = previous;
      }
    });
  });

  describe('parseStackTrace / parseStack', () => {
    it('parses error stack trace', () => {
      const error = new Error('test');
      const stack = parseStackTrace(error);

      expect(stack.length).toBeGreaterThan(0);
      expect(stack[0].filePath).toContain('stack.test.ts');
    });

    it('returns empty array for error without stack', () => {
      const error = new Error('test');
      error.stack = undefined;
      const stack = parseStackTrace(error);

      expect(stack).toEqual([]);
    });

    it('includes function names in stack when available', () => {
      function namedFunction() {
        return parseStackTrace(new Error('test'));
      }
      const stack = namedFunction();

      // Look for any entry with the function name
      const hasNamedFunction = stack.some((e) => e.functionName.includes('namedFunction'));
      expect(hasNamedFunction).toBe(true);
    });

    it('parses line and column numbers correctly', () => {
      const stack = parseStackTrace(new Error('test'));
      if (stack.length > 0) {
        expect(stack[0].lineNumber).toBeGreaterThan(0);
        expect(stack[0].columnNumber).toBeGreaterThan(0);
      }
    });

    it('parses frames whose file starts with a Windows drive', () => {
      const error = new Error('test');
      error.stack = WINDOWS_STACK;
      expect(parseStackTrace(error)).toEqual([
        {
          functionName: 'getExternalCaller',
          filePath: 'C:\\w\\src\\typescript\\framework\\utils\\src\\server\\stack.utils.ts',
          lineNumber: 70,
          columnNumber: 19,
        },
        {
          functionName: 'api',
          filePath: 'C:\\w\\src\\typescript\\framework\\application\\src\\api\\api.plugin.ts',
          lineNumber: 843,
          columnNumber: 20,
        },
        {
          functionName: '',
          filePath: 'C:\\Users\\dev\\app\\node_modules\\@putnami\\application\\src\\index.ts',
          lineNumber: 1,
          columnNumber: 1,
        },
        {
          functionName: '<anonymous>',
          filePath: 'C:\\Users\\dev\\app\\api\\src\\index.ts',
          lineNumber: 3,
          columnNumber: 17,
        },
      ]);
    });

    it('parses a Windows drive written with forward slashes', () => {
      const error = new Error('test');
      error.stack = 'Error\n    at main (C:/Users/dev/app/src/main.ts:5:9)';
      expect(parseStackTrace(error)[0]?.filePath).toBe('C:/Users/dev/app/src/main.ts');
    });
  });

  describe('Windows paths', () => {
    it('recognizes framework frames with either separator', () => {
      for (const path of [
        'C:\\w\\src\\typescript\\framework\\application\\src\\api\\api.plugin.ts',
        'C:\\Users\\dev\\app\\node_modules\\@putnami\\web\\src\\index.ts',
        'C:\\w\\src\\tooling\\cli\\x.ts',
        '/w/src/typescript/framework/application/src/api/api.plugin.ts',
      ]) {
        expect(isPutnamiInternalPath(path)).toBe(true);
      }
      expect(isPutnamiInternalPath('C:\\Users\\dev\\app\\api\\src\\index.ts')).toBe(false);
    });

    it('finds the application frame below framework frames of a Windows stack', () => {
      expect(withStack(WINDOWS_STACK, () => getExternalCaller())).toMatchObject({
        functionName: '<anonymous>',
        filePath: 'C:\\Users\\dev\\app\\api\\src\\index.ts',
        lineNumber: 3,
      });
    });
  });
});
