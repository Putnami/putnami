import { describe, expect, it } from 'bun:test';
import { ContainerValidationError, RequirementNotMetError, ScopeViolationError } from '../../src/inject/errors';
import { named } from '../../src/inject/token';

describe('ScopeViolationError', () => {
  it('should include singleton and scoped token names in message', () => {
    class SingletonService {}
    class ScopedService {}
    const error = new ScopeViolationError(SingletonService, ScopedService);

    expect(error.name).toBe('ScopeViolationError');
    expect(error.message).toContain('SingletonService');
    expect(error.message).toContain('ScopedService');
    expect(error.message).toContain('Scope violation');
    expect(error.singletonToken).toBe(SingletonService);
    expect(error.scopedToken).toBe(ScopedService);
  });

  it('should work with named tokens', () => {
    const singleton = named('MySingleton');
    const scoped = named('MyScoped');
    const error = new ScopeViolationError(singleton, scoped);

    expect(error.message).toContain('MySingleton');
    expect(error.message).toContain('MyScoped');
  });
});

describe('RequirementNotMetError', () => {
  it('should include token name and module name', () => {
    const token = named('Database');
    const error = new RequirementNotMetError(token, 'UserModule');

    expect(error.name).toBe('RequirementNotMetError');
    expect(error.message).toContain('Database');
    expect(error.message).toContain('UserModule');
    expect(error.token).toBe(token);
    expect(error.moduleName).toBe('UserModule');
  });
});

describe('ContainerValidationError', () => {
  it('should format multiple issues', () => {
    const issues = [
      { type: 'missing-dep' as const, message: 'Database not found', token: named('Database') },
      { type: 'circular' as const, message: 'A → B → A', token: named('A') },
    ];
    const error = new ContainerValidationError(issues);

    expect(error.name).toBe('ContainerValidationError');
    expect(error.message).toContain('2 issue(s)');
    expect(error.message).toContain('Database not found');
    expect(error.message).toContain('A → B → A');
    expect(error.issues).toBe(issues);
  });
});
