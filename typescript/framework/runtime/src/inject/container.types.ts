import type { Token, ValidationIssue } from './inject.type';

/**
 * Callback interface for DI debug events.
 * Used by ContainerContext when `debug: true` is set.
 * @internal
 */
export interface DiDebugLogger {
  onResolve(token: Token, container: string, cached: boolean): void;
  onScopeProxy(singletonToken: Token, scopedToken: Token): void;
  onScopeCreate(): void;
  onScopeClose(): void;
}

/**
 * Options for `describe()`.
 */
export interface DescribeOptions {
  /**
   * When `true`, runs validation and includes issues in the description.
   * Gives a single-call view of the container graph + any problems.
   */
  validate?: boolean;
}

/**
 * Debug description of a container's state.
 */
export interface ContainerDescription {
  name: string;
  closed: boolean;
  providers: ProviderDescription[];
  children: ContainerDescription[];
  /** Validation issues, present when `describe({ validate: true })` is called. */
  issues?: ValidationIssue[];
}

/**
 * Debug description of a single provider.
 */
export interface ProviderDescription {
  token: string;
  scope: string;
  visibility: string;
  async: boolean;
  lazy: boolean;
  dynamic: boolean;
  tags: string[];
  deps: string[];
  resolved: boolean;
}
