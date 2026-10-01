import { endpoint } from '@putnami/application';
import { UserService } from '../../services';

// GET /users — List users via request-scoped UserService
export const GET = endpoint()
  .inject({ userService: UserService })
  .handle((ctx) => ctx.deps.userService.listAll());
