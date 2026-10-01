import type { Provider, Token, ValidationIssue } from './inject.type';
import { tokenName } from './token';

/**
 * Validates all providers for missing dependencies and scope violations.
 * Called per-container; the caller handles recursion into children.
 */
export function validateProviders(
  providers: Map<Token, Provider>,
  containerName: string,
  hasToken: (token: Token, fromChild: boolean) => boolean,
  findProvider: (token: Token) => Provider | undefined,
): ValidationIssue[] {
  const issues: ValidationIssue[] = [];

  for (const [, provider] of providers) {
    for (const dep of provider.deps) {
      if (!hasToken(dep, false)) {
        issues.push({
          type: 'missing-dependency',
          message: `${tokenName(provider.token)} depends on ${tokenName(dep)} which is not registered in '${containerName}'`,
          tokens: [provider.token, dep],
          container: containerName,
        });
      }
    }

    // Check scope violations
    for (const dep of provider.deps) {
      const depProvider = findProvider(dep);
      if (depProvider && depProvider.scope === 'scoped' && provider.scope === 'singleton') {
        issues.push({
          type: 'scope-violation',
          message:
            `Singleton ${tokenName(provider.token)} depends on scoped ${tokenName(dep)}. ` +
            'A scope proxy will be created automatically.',
          tokens: [provider.token, dep],
          container: containerName,
        });
      }
    }
  }

  detectCycles(providers, containerName, issues);
  return issues;
}

/**
 * Detects circular dependencies using DFS-based cycle detection.
 */
function detectCycles(providers: Map<Token, Provider>, containerName: string, issues: ValidationIssue[]): void {
  const visiting = new Set<Token>();
  const visited = new Set<Token>();

  const visit = (token: Token, chain: Token[]): void => {
    if (visited.has(token)) return;
    if (visiting.has(token)) {
      const cycleStart = chain.indexOf(token);
      const cycle = [...chain.slice(cycleStart), token];
      issues.push({
        type: 'circular-dependency',
        message: `Circular dependency: ${cycle.map(tokenName).join(' → ')}`,
        tokens: cycle,
        container: containerName,
      });
      return;
    }

    visiting.add(token);
    chain.push(token);

    const provider = providers.get(token);
    if (provider) {
      for (const dep of provider.deps) {
        // Only check deps that are in this container (cross-container deps are parent-resolved)
        if (providers.has(dep)) {
          visit(dep, [...chain]);
        }
      }
    }

    visiting.delete(token);
    visited.add(token);
  };

  for (const [token] of providers) {
    visit(token, []);
  }
}
