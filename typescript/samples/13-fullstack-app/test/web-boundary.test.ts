import { describe, expect, it } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';
import { ProjectService } from '../src/projects/project.service';
import tasksAction from '../src/tasks/web/action';
import tasksLoader from '../src/tasks/web/loader';
import { TaskService } from '../src/tasks/task.service';

describe('fullstack web boundary', () => {
  it('runs the tasks loader and mutation action through request-scoped DI without a database', async () => {
    const projects = [{ id: 'project-1', name: 'Release', description: '', status: 'active' }];
    const task = {
      id: 'task-1',
      projectId: 'project-1',
      title: 'Review release',
      description: '',
      status: 'todo',
      priority: 1,
      assignee: 'ops',
      createdAt: '2026-08-17T00:00:00.000Z',
      updatedAt: '2026-08-17T00:00:00.000Z',
    };
    const updates: Array<{ id: string; status: string | undefined }> = [];

    const projectService = {
      listProjects: async () => projects,
    } as unknown as ProjectService;
    const taskService = {
      listRecentTasks: async () => [task],
      getTask: async (id: string) => (id === task.id ? task : undefined),
      updateTask: async (id: string, update: { status?: string }) => {
        updates.push({ id, status: update.status });
        return { ...task, ...update };
      },
    } as unknown as TaskService;

    const container = new ContainerContext('fullstack-web-test');
    container.register(provide(ProjectService, () => projectService));
    container.register(provide(TaskService, () => taskService));
    await container.start();

    try {
      await container.scope(async () => {
        const loaded = await tasksLoader.handler({} as never);
        expect(loaded).toEqual({
          tasks: [task],
          projectNames: { 'project-1': 'Release' },
        });

        const actionResult = await tasksAction.handler({
          body: async () => ({ taskId: task.id }),
        } as never);
        expect(actionResult).toEqual({ task: { ...task, status: 'in_progress' } });
        expect(updates).toEqual([{ id: task.id, status: 'in_progress' }]);
      });
    } finally {
      await container.close();
    }
  });
});
