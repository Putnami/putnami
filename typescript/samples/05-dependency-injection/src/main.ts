import { application, api, http, logger } from '@putnami/application';
import { DatabaseService } from './services/database.service';
import { RequestContextService } from './services/request-context.service';
import { UserService } from './services/user.service';

export const app = () =>
  application()
    .use(http())
    .use(logger())
    .provide(DatabaseService)
    .provide(RequestContextService, { scope: 'scoped' })
    .provide(UserService, { scope: 'scoped', deps: [DatabaseService, RequestContextService] })
    .use(api());
