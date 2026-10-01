import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import net from 'node:net';
import { analytics } from '@putnami/analytics';
import { config } from '@putnami/application';
import { createTestApp, type TestApp, waitFor } from '@putnami/application/testing';
import { events } from '@putnami/events';
import { database, provideRepository, repositoryToken, sql } from '@putnami/database';
import { analyticsEvents } from '../src/analytics';
import seedTask from '../src/events/project-created.seed-task.on';
import syncProject from '../src/events/task-status.sync-project.on';
import { ProjectService, projects } from '../src/projects';
import { TaskService, Tasks, tasks } from '../src/tasks';

// Skip tests when PostgreSQL is not reachable (requires running database on port 6543)
const dbAvailable = await new Promise<boolean>((resolve) => {
  const socket = new net.Socket();
  socket.setTimeout(1000);
  socket.connect(6543, 'localhost', () => {
    socket.destroy();
    resolve(true);
  });
  socket.on('error', () => {
    socket.destroy();
    resolve(false);
  });
  socket.on('timeout', () => {
    socket.destroy();
    resolve(false);
  });
});

describe.skipIf(!dbAvailable)('fullstack-app API', () => {
  let testApp: TestApp;
  let projectId: string;
  let taskId: string;

  beforeAll(async () => {
    testApp = await createTestApp({
      // `POST /api/tasks` calls `track()`, which throws when no analytics()
      // plugin is composed. A test app has to mirror the composition of the
      // application it exercises.
      plugins: [
        config(),
        sql(),
        events({ autoScan: false, handlers: [seedTask, syncProject] }),
        projects(),
        tasks(),
        analytics({ events: analyticsEvents }),
      ],
      configure: (app) => {
        app.provide(ProjectService);
        app.register(provideRepository(Tasks));
        app.provide(TaskService, { deps: [repositoryToken(Tasks)] });
      },
    });
  });

  afterAll(async () => {
    // The analytics rows this suite produced; `fullstack_db` is shared.
    const db = await database('default');
    for (const table of ['analytics_event', 'analytics_daily_counter', 'analytics_daily_visitor']) {
      await db.unsafe(`DELETE FROM ${table}`);
    }
    await testApp.stop();
  });

  it('creates a project', async () => {
    const res = await testApp.fetch('/api/projects', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'Ship Modular App', description: 'Module-first fullstack example' }),
    });

    expect(res.status).toBe(201);
    const data = await res.json();
    expect(data.project.name).toBe('Ship Modular App');
    expect(data.project.id).toBeString();
    projectId = data.project.id;
  });

  it('seeds an onboarding task from project.created event', async () => {
    const seededTask = await waitFor(async () => {
      const res = await testApp.fetch(`/api/tasks?projectId=${projectId}`);
      if (res.status !== 200) return undefined;
      const data = await res.json();
      return data.tasks.find((task: { title: string }) => task.title.includes('Kickoff')) as
        | { id: string; title: string }
        | undefined;
    });

    expect(seededTask).toBeDefined();
  });

  it('creates and updates a task through the independent tasks API', async () => {
    const createRes = await testApp.fetch('/api/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        projectId,
        title: 'Finalize launch checklist',
        description: 'Track release blockers',
        priority: 2,
        assignee: 'ops',
      }),
    });

    expect(createRes.status).toBe(201);
    const createData = await createRes.json();
    expect(createData.task.projectId).toBe(projectId);
    expect(createData.task.title).toBe('Finalize launch checklist');
    taskId = createData.task.id;

    const updateRes = await testApp.fetch(`/api/tasks/${taskId}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: 'done' }),
    });

    expect(updateRes.status).toBe(200);
    const updateData = await updateRes.json();
    expect(updateData.task.status).toBe('done');
  });

  it('syncs project status from task progress events', async () => {
    const listRes = await testApp.fetch(`/api/tasks?projectId=${projectId}`);
    expect(listRes.status).toBe(200);
    const listData = await listRes.json();

    for (const task of listData.tasks) {
      if (task.status !== 'done') {
        await testApp.fetch(`/api/tasks/${task.id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ status: 'done' }),
        });
      }
    }

    const project = await waitFor(async () => {
      const res = await testApp.fetch(`/api/projects/${projectId}`);
      if (res.status !== 200) return undefined;
      const data = await res.json();
      return data.project.status === 'completed' ? data.project : undefined;
    });

    expect(project).toBeDefined();
    expect(project.status).toBe('completed');
  });

  it('exposes module activity feeds', async () => {
    const projectsRes = await testApp.fetch('/api/projects/events?limit=20');
    expect(projectsRes.status).toBe(200);
    const projectsEvents = await projectsRes.json();
    expect(projectsEvents.events.length).toBeGreaterThan(0);

    const tasksRes = await testApp.fetch('/api/tasks/events?limit=20');
    expect(tasksRes.status).toBe(200);
    const tasksEvents = await tasksRes.json();
    expect(tasksEvents.events.length).toBeGreaterThan(0);
  });
});
