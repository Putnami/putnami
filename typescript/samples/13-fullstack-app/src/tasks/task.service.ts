import { getPublisher } from '@putnami/events';
import type { Repository } from '@putnami/database';
import { publishActivity } from '../shared/activity.feed';
import type { Tasks, Task } from './tasks.entity';
import { TaskCreated, TaskStatusChanged } from './tasks.topics';

export type { Task };

const publishTaskCreated = getPublisher(TaskCreated);
const publishTaskStatusChanged = getPublisher(TaskStatusChanged);

export class TaskService {
  /**
   * The repository arrives from DI (`provideRepository(Tasks)` in `main.ts`), so
   * the composition declares which table this service reads and writes instead
   * of hiding it inside a `new Repository(...)` the container never sees.
   */
  constructor(private repo: Repository<typeof Tasks>) {}

  async listTasks(projectId: string): Promise<Task[]> {
    return this.repo.find({ projectId }, { orderBy: 'priority ASC' });
  }

  async listRecentTasks(limit = 100): Promise<Task[]> {
    const tasks = await this.repo.find({}, { orderBy: 'updatedAt DESC' });
    return tasks.slice(0, Math.max(1, limit));
  }

  async getTask(taskId: string): Promise<Task | undefined> {
    return this.repo.get({ id: taskId });
  }

  async ensureSystemTask(projectId: string, title: string): Promise<Task> {
    const existing = await this.repo.find({ projectId, title }, { orderBy: 'createdAt ASC' });
    if (existing.length > 0) {
      return existing[0];
    }

    return this.createTask(
      projectId,
      {
        title,
        description: 'Generated automatically from module events',
        priority: 0,
        assignee: 'system',
      },
      'system',
    );
  }

  async createTask(
    projectId: string,
    data: { title: string; description: string; priority: number; assignee: string },
    source = 'user',
  ): Promise<Task> {
    const task = await this.repo.save(
      {
        id: crypto.randomUUID(),
        projectId,
        ...data,
        status: 'todo',
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      },
      { strict: true },
    );

    await publishTaskCreated({
      taskId: task.id,
      projectId: task.projectId,
      title: task.title,
      status: task.status,
      priority: task.priority,
      source,
    });

    publishActivity('tasks', {
      type: 'task.created',
      message: `Task "${task.title}" created`,
      projectId: task.projectId,
      taskId: task.id,
    });

    return task;
  }

  async updateTask(
    taskId: string,
    data: Partial<Pick<Task, 'title' | 'status' | 'priority' | 'assignee'>>,
  ): Promise<Task | undefined> {
    const task = await this.repo.get({ id: taskId });
    if (!task) return undefined;
    const updated = await this.repo.save({
      ...task,
      ...data,
      createdAt: String(task.createdAt),
      updatedAt: new Date().toISOString(),
    });

    publishActivity('tasks', {
      type: 'task.updated',
      message: `Task "${updated.title}" updated`,
      projectId: updated.projectId,
      taskId: updated.id,
    });

    if (data.status !== undefined && data.status !== task.status) {
      const progress = await this.getProjectProgress(updated.projectId);
      await publishTaskStatusChanged({
        taskId: updated.id,
        projectId: updated.projectId,
        status: updated.status,
        totalTasks: progress.totalTasks,
        remainingOpenTasks: progress.remainingOpenTasks,
      });
    }

    return updated;
  }

  async getProjectProgress(projectId: string): Promise<{ totalTasks: number; remainingOpenTasks: number }> {
    const tasks = await this.listTasks(projectId);
    const totalTasks = tasks.length;
    const remainingOpenTasks = tasks.filter((task) => task.status !== 'done').length;
    return { totalTasks, remainingOpenTasks };
  }
}
