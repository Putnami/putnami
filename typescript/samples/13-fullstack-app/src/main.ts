import { analytics } from '@putnami/analytics';
import { application, config, logger, openapi, platform, type Plugin } from '@putnami/application';
import { provideRepository, repositoryToken, sql, sqlSourceInline } from '@putnami/database';
import { events } from '@putnami/events';
import type { MigrationContributor } from '@putnami/migration';
import { react } from '@putnami/web';
import { analyticsEvents } from './analytics';
import { ProjectService, projects, projectsMigration } from './projects';
import { TaskService, Tasks, tasks } from './tasks';

export const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({
      namespace: 'app',
      datasource: { name: 'default', schema: 'public' },
      definitions: [projectsMigration],
    }),
  ],
};

export const app = () =>
  application()
    .use(logger())
    .use(config())
    .use(platform())
    .use(openapi({ title: 'Project Tracker', version: '1.0.0' }))
    .use(sql({ autoApply: true }))
    .use(migrations)
    // After sql(): the analytics tables are this plugin's own migration source.
    // `datasource` names the one database this sample has. Left out, it would
    // default to `analytics` — which resolves back to `default` at runtime, but
    // asks the deployment for a second database in infra/requirements.json.
    .use(analytics({ datasource: 'default', events: analyticsEvents }))
    .use(events())
    .provide(ProjectService)
    // The task service takes its repository from DI, so the composition names
    // the table it reads and writes.
    .register(provideRepository(Tasks))
    .provide(TaskService, { deps: [repositoryToken(Tasks)] })
    .use(tasks())
    .use(projects())
    .use(react());
