import { describe, expect, it } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';
import { DatabaseService } from '../src/services/database.service';
import { RequestContextService } from '../src/services/request-context.service';
import { UserService } from '../src/services/user.service';

function createContext() {
  const ctx = new ContainerContext('test');
  ctx.register(provide(DatabaseService));
  ctx.register(provide(RequestContextService, { scope: 'scoped' }));
  ctx.register(provide(UserService, { scope: 'scoped', deps: [DatabaseService, RequestContextService] }));
  return ctx;
}

describe('UserService', () => {
  it('should inject dependencies and resolve correctly', async () => {
    const ctx = createContext();
    await ctx.start();

    await ctx.scope(async (scope) => {
      const userService = scope.get(UserService);
      expect(userService).toBeDefined();

      const result = userService.findById('123');
      expect(result.user.id).toBe('123');
      expect(result.meta.operation).toBeGreaterThan(0);
      expect(result.meta.requestId).toBeDefined();

      const list = userService.listAll();
      expect(list.users.length).toBeGreaterThan(0);
      expect(list.meta.totalDbQueries).toBeGreaterThan(1);
    });

    await ctx.close();
  });

  it('should create separate request contexts per scope', async () => {
    const ctx = createContext();
    await ctx.start();

    let requestId1 = '';
    let requestId2 = '';

    await ctx.scope(async (scope) => {
      const userService = scope.get(UserService);
      requestId1 = userService.findById('1').meta.requestId;
    });

    await ctx.scope(async (scope) => {
      const userService = scope.get(UserService);
      requestId2 = userService.findById('1').meta.requestId;
    });

    expect(requestId1).not.toBe(requestId2);

    await ctx.close();
  });

  it('should share singleton across scopes', async () => {
    const ctx = createContext();
    await ctx.start();

    let queriesBefore = 0;

    await ctx.scope(async (scope) => {
      const userService = scope.get(UserService);
      userService.findById('1');
      const db = scope.get(DatabaseService);
      queriesBefore = db.getTotalQueries();
    });

    await ctx.scope(async (scope) => {
      const db = scope.get(DatabaseService);
      expect(db.getTotalQueries()).toBe(queriesBefore);
    });

    await ctx.close();
  });
});
