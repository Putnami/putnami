# API Reference

Complete API documentation for `@putnami/utils`. All functions are organized by category.

## Object Utilities

### `getDeep(obj, path)`

Retrieves a deeply nested value from an object using dot notation or array path.

**Type:**
```typescript
function getDeep(
  object: unknown,
  path: string | (string | number)[],
): unknown
```

**Parameters:**
- `object` - The source object to query
- `path` - The path to the property (dot notation string or array of keys)

**Returns:** The value at the specified path, or `undefined` if not found

**Example:**
```typescript
import { getDeep } from '@putnami/utils';

const user = { profile: { name: 'Alice', tags: ['admin', 'user'] } };

getDeep(user, 'profile.name');      // 'Alice'
getDeep(user, 'profile.tags[0]');   // 'admin'
getDeep(user, ['profile', 'name']); // 'Alice'
getDeep(user, 'invalid.path');      // undefined
```

**Supports:**
- Dot notation: `'user.address.city'`
- Array notation: `['user', 'address', 'city']`
- Mixed notation: `'user.tags[0]'`

---

### `mergeDeep(a, b)`

Recursively merges two objects, with properties from the second object taking precedence over the first. Arrays are overwritten, not merged.

**Type:**
```typescript
function mergeDeep<A, B>(a: A, b: B): A & B
```

**Parameters:**
- `a` - The base object
- `b` - The object to merge into the base

**Returns:** A new object with deeply merged properties. Plain nested objects and arrays are copied so the input objects are not mutated by the merge.

**Example:**
```typescript
import { mergeDeep } from '@putnami/utils';

const defaults = { theme: { color: 'blue', size: 'md' } };
const overrides = { theme: { color: 'red' } };

mergeDeep(defaults, overrides);
// { theme: { color: 'red', size: 'md' } }
```

---

### `sortObjectKeys(obj)`

Sorts the keys of an object alphabetically, returning a new object with the same key-value pairs in sorted order.

**Type:**
```typescript
function sortObjectKeys<T extends Record<string, unknown>>(
  obj?: T
): T | undefined
```

**Parameters:**
- `obj` - The object whose keys should be sorted

**Returns:** A new object with alphabetically sorted keys, or `undefined` if input is falsy

**Example:**
```typescript
import { sortObjectKeys } from '@putnami/utils';

sortObjectKeys({ z: 1, a: 2, m: 3 });
// { a: 2, m: 3, z: 1 }

// Note: Only top-level keys are sorted
sortObjectKeys({ dependencies: { react: '^18.0.0', axios: '^1.0.0' } });
// { dependencies: { react: '^18.0.0', axios: '^1.0.0' } }
```

---

### `omitUndefinedValues(obj)`

Creates a new object with all `undefined` values removed. Recursively processes nested objects but preserves arrays, dates, and null values.

**Type:**
```typescript
function omitUndefinedValues<T extends object>(obj: T): T
```

**Parameters:**
- `obj` - The object to process

**Returns:** A new object without `undefined` values

**Example:**
```typescript
import { omitUndefinedValues } from '@putnami/utils';

omitUndefinedValues({ a: 1, b: undefined, c: { d: undefined, e: 2 } });
// { a: 1, c: { e: 2 } }

omitUndefinedValues({ date: new Date(), value: null });
// { date: Date, value: null } - Dates and null are preserved
```

---

## Array Utilities

### `ensureArray(value)`

Ensures a value is wrapped in an array. If the value is already an array, it is returned as-is.

**Type:**
```typescript
function ensureArray<T>(value: T | T[]): T[]
```

**Parameters:**
- `value` - The value or array to process

**Returns:** An array containing the value(s)

**Example:**
```typescript
import { ensureArray } from '@putnami/utils';

ensureArray('single');        // ['single']
ensureArray(['a', 'b']);      // ['a', 'b']
ensureArray(undefined);       // [undefined]
ensureArray([]);              // []
```

---

### `first(arr)`

Returns the first element of an array, or undefined if the array is empty.

**Type:**
```typescript
function first<T>(arr: T[]): T | undefined
```

**Parameters:**
- `arr` - The array to get the first element from

**Returns:** The first element or undefined

**Example:**
```typescript
import { first } from '@putnami/utils';

first([1, 2, 3]);     // 1
first([]);            // undefined
first(['a', 'b']);    // 'a'
```

---

### `last(arr)`

Returns the last element of an array, or undefined if the array is empty.

**Type:**
```typescript
function last<T>(arr: T[]): T | undefined
```

**Parameters:**
- `arr` - The array to get the last element from

**Returns:** The last element or undefined

**Example:**
```typescript
import { last } from '@putnami/utils';

last([1, 2, 3]);     // 3
last([]);            // undefined
last(['a', 'b']);    // 'b'
```

---

### `unique(arr)`

Removes duplicate values from an array.

**Type:**
```typescript
function unique<T>(arr: T[]): T[]
```

**Parameters:**
- `arr` - The array to deduplicate

**Returns:** A new array with unique values

**Example:**
```typescript
import { unique } from '@putnami/utils';

unique([1, 2, 2, 3, 1]);     // [1, 2, 3]
unique(['a', 'b', 'a']);     // ['a', 'b']
```

---

### `chunk(arr, size)`

Chunks an array into smaller arrays of a specified size.

**Type:**
```typescript
function chunk<T>(arr: T[], size: number): T[][]
```

**Parameters:**
- `arr` - The array to chunk
- `size` - The size of each chunk

**Returns:** An array of chunks

**Example:**
```typescript
import { chunk } from '@putnami/utils';

chunk([1, 2, 3, 4, 5], 2);   // [[1, 2], [3, 4], [5]]
chunk([1, 2, 3], 3);         // [[1, 2, 3]]
chunk([], 2);                // []
```

---

## Type Utilities

### `isString(value)`

Checks if a value is a string (primitive or String object).

**Type:**
```typescript
function isString(value?: unknown): value is string
```

**Parameters:**
- `value` - The value to check

**Returns:** `true` if the value is a string, `false` otherwise

**Example:**
```typescript
import { isString } from '@putnami/utils';

isString('hello');          // true
isString(new String('hi')); // true
isString(123);              // false
isString(null);             // false
```

---

### `isObject(value)`

Checks if a value is an object (including functions, excluding null).

**Type:**
```typescript
function isObject(value: unknown): value is object
```

**Parameters:**
- `value` - The value to check

**Returns:** `true` if the value is an object or function, `false` otherwise

**Example:**
```typescript
import { isObject } from '@putnami/utils';

isObject({});              // true
isObject([]);              // true
isObject(() => {});        // true
isObject(null);            // false
isObject(undefined);       // false
isObject('string');        // false
```

---

### `isPlainObject(value)`

Checks if a value is a plain object (not an array, date, or other special object).

**Type:**
```typescript
function isPlainObject(value: unknown): value is Record<string, unknown>
```

**Parameters:**
- `value` - The value to check

**Returns:** `true` if the value is a plain object, `false` otherwise

**Example:**
```typescript
import { isPlainObject } from '@putnami/utils';

isPlainObject({});           // true
isPlainObject({ a: 1 });     // true
isPlainObject([]);           // false
isPlainObject(new Date());   // false
isPlainObject(null);         // false
```

---

### `isNullish(value)`

Checks if a value is null or undefined.

**Type:**
```typescript
function isNullish(value: unknown): value is null | undefined
```

**Parameters:**
- `value` - The value to check

**Returns:** `true` if the value is null or undefined, `false` otherwise

**Example:**
```typescript
import { isNullish } from '@putnami/utils';

isNullish(null);        // true
isNullish(undefined);   // true
isNullish(0);           // false
isNullish('');          // false
isNullish(false);       // false
```

---

### `isFunction(value)`

Checks if a value is a function.

**Type:**
```typescript
function isFunction(value: unknown): value is Function
```

**Parameters:**
- `value` - The value to check

**Returns:** `true` if the value is a function, `false` otherwise

**Example:**
```typescript
import { isFunction } from '@putnami/utils';

isFunction(() => {});           // true
isFunction(function() {});      // true
isFunction(class {});           // true
isFunction({});                 // false
```

---

### `throwError(message)`

Throws an error with the given message. This function never returns.

**Type:**
```typescript
function throwError(message: string): never
```

**Parameters:**
- `message` - The error message

**Throws:** Always throws an Error with the given message

**Returns:** Never returns (always throws)

**Example:**
```typescript
import { throwError } from '@putnami/utils';

// Inline error throwing in expressions
const value = maybeValue ?? throwError('Value is required');

// Exhaustive switch checking
switch (status) {
  case 'pending': return handlePending();
  case 'done': return handleDone();
  default: throwError(`Unknown status: ${status}`);
}
```

---

### `Promisable<T>`

Type alias for values that can be a promise or a value.

**Type:**
```typescript
type Promisable<T> = T | Promise<T>
```

**Example:**
```typescript
import type { Promisable } from '@putnami/utils';

function processData(data: Promisable<string>): Promise<string> {
  return Promise.resolve(data);
}
```

---

## URL Utilities

### `buildUrlWithParams(baseUrl, params)`

Builds a URL with query parameters and path parameter substitution.

**Type:**
```typescript
function buildUrlWithParams(
  baseUrl: string,
  params?: Record<string, unknown>
): string
```

**Parameters:**
- `baseUrl` - The base URL, optionally containing path parameters like `[id]`
- `params` - Object of parameters to substitute/append

**Returns:** The fully constructed URL with parameters

**Example:**
```typescript
import { buildUrlWithParams } from '@putnami/utils';

// Query parameters
buildUrlWithParams('https://api.example.com/users', { page: 1, limit: 10 });
// 'https://api.example.com/users?page=1&limit=10'

// Path parameters
buildUrlWithParams('https://api.example.com/users/[id]', { id: '123' });
// 'https://api.example.com/users/123'

// Mixed
buildUrlWithParams('https://api.example.com/users/[id]/posts', { id: '123', page: 1 });
// 'https://api.example.com/users/123/posts?page=1'
```

**Supports:**
- Path parameters: `[paramName]` in URL
- Query parameters: Other params appended as query string
- Null/undefined values are skipped

---

### `parseQueryString(query)`

Parses a query string into an object of key-value pairs.

**Type:**
```typescript
function parseQueryString(query: string): Record<string, string>
```

**Parameters:**
- `query` - The query string to parse (with or without leading `?`)

**Returns:** An object with the parsed query parameters

**Example:**
```typescript
import { parseQueryString } from '@putnami/utils';

parseQueryString('page=1&limit=10');
// { page: '1', limit: '10' }

parseQueryString('?search=hello%20world');
// { search: 'hello world' }

parseQueryString('');
// {}
```

---

### `getBaseUrl(url)`

Extracts the base URL (origin + pathname) from a full URL, removing query and hash.

**Type:**
```typescript
function getBaseUrl(url: string): string
```

**Parameters:**
- `url` - The full URL

**Returns:** The base URL without query string or hash

**Example:**
```typescript
import { getBaseUrl } from '@putnami/utils';

getBaseUrl('https://example.com/path?query=1#hash');
// 'https://example.com/path'

getBaseUrl('https://example.com');
// 'https://example.com'
```

---

### `joinUrlPath(...segments)`

Joins URL path segments, ensuring proper slash handling.

**Type:**
```typescript
function joinUrlPath(...segments: string[]): string
```

**Parameters:**
- `segments` - Path segments to join

**Returns:** The joined URL path

**Example:**
```typescript
import { joinUrlPath } from '@putnami/utils';

joinUrlPath('https://api.example.com', 'users', '123');
// 'https://api.example.com/users/123'

joinUrlPath('/api/', '/users/', '/list');
// '/api/users/list'
```

---

## Task Utilities

### `TaskQueue`

A concurrent task processor that limits the number of simultaneous async operations.

**Type:**
```typescript
class TaskQueue {
  constructor(concurrency?: number);
  process<T>(task: () => Promise<T>): Promise<T>;
}
```

**Constructor:**
- `concurrency` - Maximum number of tasks that can run simultaneously (default: 10)

**Methods:**

#### `process(task)`

Adds a task to the queue and returns a promise that resolves when the task completes.

**Type:**
```typescript
process<T>(task: () => Promise<T>): Promise<T>
```

**Parameters:**
- `task` - The async task function to execute

**Returns:** A promise that resolves with the task result

**Example:**
```typescript
import { TaskQueue } from '@putnami/utils';

// Create a queue with max 3 concurrent tasks
const queue = new TaskQueue(3);

// Add tasks to the queue
const results = await Promise.all([
  queue.process(() => fetch('/api/user/1').then(r => r.json())),
  queue.process(() => fetch('/api/user/2').then(r => r.json())),
  queue.process(() => fetch('/api/user/3').then(r => r.json())),
  queue.process(() => fetch('/api/user/4').then(r => r.json())), // Waits for a slot
  queue.process(() => fetch('/api/user/5').then(r => r.json())), // Waits for a slot
]);
```

---

## Path Utilities (Server Only)

> **Note:** These utilities are only available in server environments (Bun). See [Browser vs Server](./browser-vs-server.md) for details.

### `joinPath(...paths)`

Joins path segments using the platform-specific separator.

**Type:**
```typescript
function joinPath(...paths: string[]): string
```

**Parameters:**
- `paths` - Path segments to join

**Returns:** The joined path

**Example:**
```typescript
import { joinPath } from '@putnami/utils';

joinPath('folder', 'subfolder', 'file.txt');
// 'folder/subfolder/file.txt' (on Unix)
// 'folder\\subfolder\\file.txt' (on Windows)
```

---

### `relativePath(from, to)`

Computes the relative path from one path to another.

**Type:**
```typescript
function relativePath(from: string, to: string): string
```

**Parameters:**
- `from` - The starting path
- `to` - The destination path

**Returns:** The relative path from `from` to `to`

**Example:**
```typescript
import { relativePath } from '@putnami/utils';

relativePath('/path/to/dir', '/path/to/dir/file.txt');
// 'file.txt'

relativePath('/path/to/dir', '/path/other');
// '../other'
```

---

### `getDirectoryName(path)`

Returns the directory name of a path.

**Type:**
```typescript
function getDirectoryName(path: string): string
```

**Parameters:**
- `path` - The file path

**Returns:** The directory containing the path

**Example:**
```typescript
import { getDirectoryName } from '@putnami/utils';

getDirectoryName('/path/to/file.txt');
// '/path/to'

getDirectoryName('file.txt');
// '.'
```

---

### `resolvePath(...paths)`

Resolves a sequence of paths or path segments into an absolute path.

**Type:**
```typescript
function resolvePath(...paths: string[]): string
```

**Parameters:**
- `paths` - Path segments to resolve

**Returns:** The resolved absolute path

**Example:**
```typescript
import { resolvePath } from '@putnami/utils';

resolvePath('/path', 'to', 'file.txt');
// '/path/to/file.txt'

resolvePath('relative', 'path');
// '/current/working/dir/relative/path'
```

---

### `getBaseName(path, suffix?)`

Returns the last portion of a path (the filename).

**Type:**
```typescript
function getBaseName(path: string, suffix?: string): string
```

**Parameters:**
- `path` - The file path
- `suffix` - Optional suffix to remove from the result

**Returns:** The filename portion of the path

**Example:**
```typescript
import { getBaseName } from '@putnami/utils';

getBaseName('/path/to/file.txt');
// 'file.txt'

getBaseName('/path/to/file.txt', '.txt');
// 'file'
```

---

### `getExtension(path)`

Returns the extension of a path.

**Type:**
```typescript
function getExtension(path: string): string
```

**Parameters:**
- `path` - The file path

**Returns:** The extension including the dot, or empty string if no extension

**Example:**
```typescript
import { getExtension } from '@putnami/utils';

getExtension('/path/to/file.txt');
// '.txt'

getExtension('/path/to/file');
// ''
```

---

### `normalizePath(path)`

Normalizes a path, resolving '..' and '.' segments.

**Type:**
```typescript
function normalizePath(path: string): string
```

**Parameters:**
- `path` - The path to normalize

**Returns:** The normalized path

**Example:**
```typescript
import { normalizePath } from '@putnami/utils';

normalizePath('/path/to/../file.txt');
// '/path/file.txt'
```

---

### `isAbsolutePath(path)`

Determines whether a path is absolute.

**Type:**
```typescript
function isAbsolutePath(path: string): boolean
```

**Parameters:**
- `path` - The path to check

**Returns:** `true` if the path is absolute

**Example:**
```typescript
import { isAbsolutePath } from '@putnami/utils';

isAbsolutePath('/path/to/file');
// true

isAbsolutePath('relative/path');
// false
```

---

### `toPosixPath(path)`

Converts every backslash in a path to a forward slash. Use it for a path written into generated source text: an import specifier, an identifier derived from a path, a route key or a manifest path. On Windows the native separator is a backslash, and inside a string literal it starts an escape sequence. Filesystem calls keep native paths.

**Type:**
```typescript
function toPosixPath(path: string): string
```

**Parameters:**
- `path` - A path in native or forward-slash form

**Returns:** The same path with forward slashes

**Example:**
```typescript
import { toPosixPath } from '@putnami/utils';

toPosixPath('..\\..\\src\\api\\get');
// '../../src/api/get'
```

---

### `joinPosixPath(...paths)`

Joins path segments with forward slashes on every platform. Segments in native Windows form are converted first. Use it to build generated source text; use `joinPath` for filesystem paths.

**Type:**
```typescript
function joinPosixPath(...paths: string[]): string
```

**Parameters:**
- `paths` - Path segments to join

**Returns:** The joined path in forward-slash form

**Example:**
```typescript
import { joinPosixPath } from '@putnami/utils';

joinPosixPath('..\\..\\src\\app', 'not-found');
// '../../src/app/not-found' (on every platform)
```

---

## Workspace Utilities (Server Only)

> **Note:** These utilities are only available in server environments (Bun). See [Browser vs Server](./browser-vs-server.md) for details.

### `getWorkspaceRoot(throwIfMissing?)`

Returns the root directory of the Putnami workspace.

**Type:**
```typescript
function getWorkspaceRoot(throwIfMissing?: boolean): string
```

**Parameters:**
- `throwIfMissing` - Whether to throw if workspace root is not found (default: `true`)

**Returns:** The absolute path to the workspace root

**Example:**
```typescript
import { getWorkspaceRoot } from '@putnami/utils';

const root = getWorkspaceRoot();
// '/Users/dev/myproject'
```

**Details:**
- Searches upward from the current directory for a `package.json` with a `putnami` field
- Can be overridden by setting the `PUTNAMI_WORKSPACE_ROOT` environment variable
- Result is cached after the first call

---

### `getProjectRoot()`

Returns the current project's root directory.

**Type:**
```typescript
function getProjectRoot(): string
```

**Returns:** The absolute path to the current project root

**Throws:** Error if `PUTNAMI_PROJECT_ROOT` or `PWD` environment variable is not set

**Example:**
```typescript
import { getProjectRoot } from '@putnami/utils';

const projectDir = getProjectRoot();
// '/Users/dev/myproject/packages/core'
```

---

### `getCurrentProject()`

Returns the current project's information.

**Type:**
```typescript
function getCurrentProject(): ProjectRef
```

**Returns:** The current project information

**Throws:** Error if no project is found at the current location

**Example:**
```typescript
import { getCurrentProject } from '@putnami/utils';

const project = getCurrentProject();
console.log(project.name);         // '@putnami/runtime'
console.log(project.dependencies); // ['@putnami/utils']
```

**Details:**
- Caches the result after the first call for performance
- Reads `package.json` from the current project root

**ProjectRef Interface:**
```typescript
interface ProjectRef {
  /** The package name from package.json */
  name: string;
  /** Absolute path to the project directory */
  path: string;
  /** List of dependency package names */
  dependencies: string[];
}
```

---

### `getProject(path)`

Gets project information by path or name.

**Type:**
```typescript
function getProject(path: string): ProjectRef | undefined
```

**Parameters:**
- `path` - The project path or name

**Returns:** The project information, or `undefined` if not found

**Example:**
```typescript
import { getProject } from '@putnami/utils';

const project = getProject('@putnami/runtime');
// { name: '@putnami/runtime', path: '/path/to/core', dependencies: [...] }

const projectByPath = getProject('/path/to/core');
// Same result
```

---

### `listProjectDependencies(name)`

Lists all dependencies (including nested) for a project.

**Type:**
```typescript
function listProjectDependencies(name: string): string[]
```

**Parameters:**
- `name` - The project name

**Returns:** Array of all dependency package names

**Example:**
```typescript
import { listProjectDependencies } from '@putnami/utils';

const deps = listProjectDependencies('@putnami/runtime');
// ['@putnami/utils', '@putnami/typescript', ...]
```

---

## Stack Utilities (Server Only)

> **Note:** These utilities are only available in server environments (Bun). See [Browser vs Server](./browser-vs-server.md) for details.

### `getCallerInfo(depth?)`

Gets information about the caller of the current function.

**Type:**
```typescript
function getCallerInfo(depth?: number): StackElement | undefined
```

**Parameters:**
- `depth` - How many additional stack frames to go up (default: 0 = immediate caller)

**Returns:** The stack element for the caller, or undefined if not found

**Example:**
```typescript
import { getCallerInfo } from '@putnami/utils';

function myFunction() {
  const caller = getCallerInfo();
  console.log(`Called from: ${caller?.filePath}:${caller?.lineNumber}`);
}
```

---

### `getExternalCaller()`

Finds the first caller from outside @putnami packages.

**Type:**
```typescript
function getExternalCaller(): StackElement | undefined
```

**Returns:** The first stack element from external code, or undefined if not found

**Example:**
```typescript
import { getExternalCaller } from '@putnami/utils';

// In a plugin factory function:
const caller = getExternalCaller();
if (caller?.filePath) {
  // caller.filePath is the user's file, not internal @putnami code
}
```

**Details:**
- More reliable than depth-based `getCallerInfo()` for bundled code
- Skips internal @putnami package paths
- Useful when function inlining changes stack trace depth

---

### `parseStackTrace(error)`

Parses an Error's stack trace into structured elements.

**Type:**
```typescript
function parseStackTrace(error: Error): StackElement[]
```

**Parameters:**
- `error` - The Error object with a stack trace to parse

**Returns:** Array of stack elements, from most recent to oldest

**Example:**
```typescript
import { parseStackTrace } from '@putnami/utils';

try {
  throw new Error('test');
} catch (err) {
  const stack = parseStackTrace(err);
  stack.forEach(elem => {
    console.log(`${elem.functionName} at ${elem.filePath}:${elem.lineNumber}`);
  });
}
```

**StackElement Interface:**
```typescript
interface StackElement {
  /** The function name at this stack level (empty string if anonymous) */
  functionName: string;
  /** The full file path */
  filePath: string;
  /** The line number (1-indexed) */
  lineNumber: number;
  /** The column number (1-indexed) */
  columnNumber: number;
}
```

**Details:**
- Handles multiple stack trace formats (V8, SpiderMonkey, etc.)
- Returns empty array if error has no stack trace

---

## Package.json Utilities (Server Only)

> **Note:** These utilities read the filesystem, so they are exported only by the
> server entry point. They are absent from `@putnami/utils` in a browser build.

### `readPackageJson(path, options?)`

Reads and caches a `package.json` file.

```typescript
import { readPackageJson } from '@putnami/utils';

const pkg = readPackageJson('/path/to/project');
console.log(pkg?.name, pkg?.version);
```

### `updatePackageJson(path, data)`

Updates a `package.json` file with partial data, sorting dependencies alphabetically.

```typescript
import { updatePackageJson } from '@putnami/utils';

updatePackageJson('/path/to/package.json', { version: '1.0.1' });
```

### `resolvePackageExportPath(exportsField, modulePath)`

Resolves an export condition from a package.json `exports` field.

```typescript
import { readPackageJson, resolvePackageExportPath } from '@putnami/utils';

const pkg = readPackageJson('/path/to/project');
const path = resolvePackageExportPath(pkg?.exports, './serve');
```

### `clearPackageJsonCache(path?)`

Clears the package.json cache (all entries or a specific path).

---

## Project Configuration (Server Only)

### `readProjectConfigFile(absoluteDirPath)`

Reads a `.putnamirc.json` project configuration file.

```typescript
import { readProjectConfigFile } from '@putnami/utils';

const config = readProjectConfigFile('/path/to/project');
console.log(config?.tags, config?.type);
```

---

## Hook Protocol (Server Only)

JSONL event protocol for extension hooks. Used by `@putnami/application` and `@putnami/web` generate hooks.

> The hook protocol is a specialized extension-system SDK and lives behind a
> dedicated subpath. Import it from `@putnami/utils/hooks` (not the root
> `@putnami/utils` barrel, which is reserved for generic helpers).

### Event emission

```typescript
import { emitLog, emitProgress, emitArtifact, emitSummary, emitError } from '@putnami/utils/hooks';

emitLog('info', 'Processing files');
emitProgress('Building', 50, 'compile');
emitArtifact('write', 'dist/bundle.js');
emitSummary('Build complete', { durationMs: 1234, outputs: 5 });
```

### Hook command runner

```typescript
import { runHookCommand, standardHookModel, type HookContext, type HookResult } from '@putnami/utils/hooks';

const model = standardHookModel.extend({
  name: 'generate',
  description: 'Generate code',
});

runHookCommand({
  model,
  extension: '@putnami/web',
  hook: 'preBuild',
  run: async (options, context: HookContext): Promise<HookResult> => {
    // ... generate code ...
    return { exports: { 'loader': '/path/to/loader.ts' }, assets: {} };
  },
});
```

---

## Next Steps

- See [Getting Started](./getting-started.md) for installation and basic usage
- Learn about [Browser vs Server](./browser-vs-server.md) conditional exports
- Check the [README](../README.md) for additional information
