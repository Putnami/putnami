import { endpoint } from '@putnami/application';
import { DatabaseService } from '../services/database.service';
import { RequestContextService } from '../services/request-context.service';
import { UserService } from '../services/user.service';

// GET / — Demonstrates DI with different scopes
export const GET = endpoint()
  .inject({
    db: DatabaseService,
    reqCtx: RequestContextService,
    userService: UserService,
  })
  .handle((ctx) => {
    const { db, reqCtx, userService } = ctx.deps;
    const user1 = userService.findById('1');
    const allUsers = userService.listAll();
    const user2 = userService.findById('2');

    return {
      message: 'DI Container Demo',
      scope: {
        singleton: {
          description: 'DatabaseService is shared across ALL requests',
          totalQueriesEver: db.getTotalQueries(),
        },
        request: {
          description: 'RequestContextService is unique to THIS request',
          requestId: reqCtx.requestId,
          elapsedMs: reqCtx.getElapsedMs(),
        },
      },
      operations: {
        user1: user1.meta,
        allUsers: allUsers.meta,
        user2: user2.meta,
      },
    };
  });
