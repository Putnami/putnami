/**
 * Stack Trace Utility Functions (Server Only)
 *
 * Utilities for parsing and analyzing JavaScript stack traces.
 * These are useful for debugging, logging, and error reporting.
 *
 * @module @putnami/utils
 */

import { isAbsolute, relative } from 'node:path';

/**
 * Represents a single element in a parsed stack trace.
 */
export interface StackElement {
  /** The function name at this stack level (empty string if anonymous) */
  functionName: string;
  /** The full file path */
  filePath: string;
  /** The line number (1-indexed) */
  lineNumber: number;
  /** The column number (1-indexed) */
  columnNumber: number;
}

/**
 * Gets information about the caller of the current function.
 *
 * Useful for debugging or logging context without passing explicit parameters.
 *
 * @param depth - How many additional stack frames to go up (default: 0 = immediate caller)
 * @returns The stack element for the caller, or undefined if not found
 *
 * @example
 * ```typescript
 * function myFunction() {
 *   const caller = getCallerInfo();
 *   console.log(`Called from: ${caller?.filePath}:${caller?.lineNumber}`);
 * }
 * ```
 */
export function getCallerInfo(depth = 0): StackElement | undefined {
  const stack = parseStackTrace(new Error());
  // Stack: [0] = Error, [1] = parseStackTrace, [2] = getCallerInfo, [3+] = callers
  // So we need index 3 + depth to get the caller
  return stack[3 + depth];
}

/**
 * Finds the first caller from outside @putnami packages.
 *
 * More reliable than depth-based `getCallerInfo()` for bundled code,
 * where function inlining changes stack trace depth.
 *
 * @param projectRoot - Optional root used to make the returned path relative;
 *   callers outside this root are ignored
 * @returns The first stack element from external code, or undefined if not found
 *
 * @example
 * ```typescript
 * // In a plugin factory function:
 * const caller = getExternalCaller();
 * if (caller?.filePath) {
 *   // caller.filePath is the user's file, not internal @putnami code
 * }
 * ```
 */
export function getExternalCaller(projectRoot?: string): StackElement | undefined {
  try {
    const stack = parseStackTrace(new Error());
    for (const frame of stack) {
      if (isPutnamiInternalPath(frame.filePath)) {
        continue;
      }
      // Skip internal stack frames (Error, parseStackTrace, etc.)
      if (!frame.filePath || frame.filePath === '<anonymous>') {
        continue;
      }
      if (!projectRoot) return frame;

      if (!isAbsolute(frame.filePath)) {
        const file = frame.filePath.replaceAll('\\', '/');
        if (file === '..' || file.startsWith('../')) return undefined;
        return { ...frame, filePath: file };
      }
      const projectRelative = relative(projectRoot, frame.filePath).replaceAll('\\', '/');
      if (projectRelative === '..' || projectRelative.startsWith('../') || isAbsolute(projectRelative)) {
        return undefined;
      }
      return { ...frame, filePath: projectRelative };
    }
    return undefined;
  } catch {
    // Provenance is optional runtime metadata. Stack implementations differ
    // across JS engines and instrumentation; failure must never break the API
    // declaration or client-construction path it annotates.
    return undefined;
  }
}

/**
 * Checks if a file path is internal to @putnami packages.
 * Works for both published packages (node_modules/@putnami/*)
 * and development workspace paths, with either separator: a Windows stack
 * frame names its file with backslashes.
 */
export function isPutnamiInternalPath(nativePath: string): boolean {
  const filePath = nativePath.replaceAll('\\', '/');
  // Published packages: node_modules/@putnami/*
  if (filePath.includes('node_modules/@putnami/')) {
    return true;
  }
  // Development workspace structure:
  // - tooling/{cli,extension-sdk}
  // - platform/{ci,...}
  // - {typescript,go,python}/extension
  // - {typescript,go,python}/framework/*
  const devPatterns = [
    /\/tooling\/(cli|extension-sdk)\//,
    /\/platform\/[^/]+\//,
    /\/(typescript|go|python)\/extension\//,
    /\/(typescript|go|python)\/framework\/[^/]+\//,
  ];
  for (const pattern of devPatterns) {
    if (pattern.test(filePath)) {
      return true;
    }
  }
  return false;
}

/**
 * Parses an Error's stack trace into structured elements.
 *
 * Handles multiple stack trace formats (V8, SpiderMonkey, etc.).
 *
 * @param error - The Error object with a stack trace to parse
 * @returns Array of stack elements, from most recent to oldest
 *
 * @example
 * ```typescript
 * try {
 *   throw new Error('test');
 * } catch (err) {
 *   const stack = parseStackTrace(err);
 *   stack.forEach(elem => {
 *     console.log(`${elem.functionName} at ${elem.filePath}:${elem.lineNumber}`);
 *   });
 * }
 * ```
 */
export function parseStackTrace(error: Error): StackElement[] {
  if (!error.stack) {
    return [];
  }

  const lines = error.stack.split('\n');

  return lines.reduce((stack, line) => {
    const parseResult = parseLine(line);
    if (parseResult) {
      stack.push(parseResult);
    }

    return stack;
  }, [] as StackElement[]);
}

/**
 * Parses a single line from a stack trace. A file path is a colon-free path,
 * optionally after a Windows drive (`C:\` or `C:/`).
 */
function parseLine(line: string): StackElement | undefined {
  // V8 format: "at functionName (filePath:lineNumber:columnNumber)"
  const regex1 = /at\s+(.*?)\s+\(((?:[A-Za-z]:[\\/])?[^:]+):(\d+):(\d+)\)/;
  let match = line.match(regex1);

  if (match) {
    return {
      functionName: match[1],
      filePath: match[2],
      lineNumber: Number(match[3]),
      columnNumber: Number(match[4]),
    };
  }

  // V8 format for anonymous: "at filePath:lineNumber:columnNumber"
  const regex2 = /at\s+(?<filename>(?:[A-Za-z]:[\\/])?[^:]+):(?<lineNumber>\d+):(?<columnNumber>\d+)$/;
  match = line.match(regex2);
  if (match) {
    return {
      functionName: '',
      filePath: match.groups?.['filename'] ?? '',
      lineNumber: Number(match.groups?.['lineNumber'] ?? -1),
      columnNumber: Number(match.groups?.['columnNumber'] ?? -1),
    };
  }

  return undefined;
}
