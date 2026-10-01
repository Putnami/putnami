import type { Token, ValidationIssue } from './inject.type';
import { tokenName } from './token';

/**
 * Base error for all DI-related errors.
 */
export class DiError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'DiError';
  }
}

/**
 * Formats a resolution chain for inclusion in error messages.
 * Shows the path of dependencies that led to the error.
 *
 * @internal
 */
function formatResolutionChain(chain: Token[]): string {
  if (chain.length === 0) return '';
  return `\n  Resolution chain: ${chain.map(tokenName).join(' → ')}`;
}

/**
 * Thrown when a token is not found in the container hierarchy.
 *
 * When resolution spans several containers (e.g. a root plus mounted modules),
 * pass `searchedContainers` so the message lists every container that was
 * checked. This keeps the hint actionable instead of naming only the root.
 */
export class NotRegisteredError extends DiError {
  constructor(
    public readonly token: Token,
    public readonly containerName: string,
    public readonly resolutionChain: Token[] = [],
    public readonly searchedContainers: string[] = [],
  ) {
    const chainInfo = formatResolutionChain(resolutionChain);
    const searchedInfo =
      searchedContainers.length > 0 ? `\n  Searched containers: ${searchedContainers.join(', ')}` : '';
    const name = tokenName(token);
    super(
      `${name} is not registered in container '${containerName}' or its parents.${searchedInfo}${chainInfo}\n` +
        `  Hint: Add .provide(${name}) to your application or module.`,
    );
    this.name = 'NotRegisteredError';
  }
}

/**
 * Thrown when a circular dependency is detected.
 * Includes the full chain for debugging.
 */
export class CircularDependencyError extends DiError {
  constructor(public readonly chain: Token[]) {
    const names = chain.map(tokenName);
    super(
      `Circular dependency detected: ${names.join(' → ')}\n` +
        '  Hint: Break the cycle by extracting shared logic into a separate service, or use a factory with lazy resolution.',
    );
    this.name = 'CircularDependencyError';
  }
}

/**
 * Thrown when a singleton provider depends on a scoped provider
 * and proxy support is not available.
 */
export class ScopeViolationError extends DiError {
  constructor(
    public readonly singletonToken: Token,
    public readonly scopedToken: Token,
  ) {
    super(
      `Scope violation: singleton ${tokenName(singletonToken)} depends on scoped ${tokenName(scopedToken)}. ` +
        'The scoped dependency will be auto-proxied to resolve from the current scope.',
    );
    this.name = 'ScopeViolationError';
  }
}

/**
 * Thrown when validation fails during `start()`.
 * Contains all validation issues.
 */
export class ContainerValidationError extends DiError {
  constructor(public readonly issues: ValidationIssue[]) {
    const summary = issues.map((i) => `  - [${i.type}] ${i.message}`).join('\n');
    super(`Container validation failed with ${issues.length} issue(s):\n${summary}`);
    this.name = 'ContainerValidationError';
  }
}

/**
 * Thrown when `get()` is called on a closed container.
 */
export class ContainerClosedError extends DiError {
  constructor(public readonly containerName: string) {
    super(`Container '${containerName}' is closed`);
    this.name = 'ContainerClosedError';
  }
}

/**
 * Thrown when the same token is registered twice in the same container.
 */
export class DuplicateProviderError extends DiError {
  constructor(
    public readonly token: Token,
    public readonly containerName: string,
  ) {
    super(
      `Duplicate provider for ${tokenName(token)} in container '${containerName}'.\n` +
        '  Hint: Each token can only be registered once per container. Use a different container or module for the second registration.',
    );
    this.name = 'DuplicateProviderError';
  }
}

/**
 * Thrown when a module's `require()` dependency is not satisfied by its parent.
 */
export class RequirementNotMetError extends DiError {
  constructor(
    public readonly token: Token,
    public readonly moduleName: string,
  ) {
    const name = tokenName(token);
    super(
      `Module '${moduleName}' requires ${name} but it is not available in the parent container.\n` +
        `  Hint: Add .provide(${name}) to your application before .use(${moduleName}).`,
    );
    this.name = 'RequirementNotMetError';
  }
}
