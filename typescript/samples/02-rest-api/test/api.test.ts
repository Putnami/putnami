import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { api, openapi, platform } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';

describe('rest-api sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [platform(), openapi({ title: 'Tasks API', version: '1.0.0' }), api({ scanPath: 'src/api' })],
    });
  });

  afterAll(async () => {
    await testApp.stop();
  });

  describe('GET /tasks', () => {
    it('should return list of tasks', async () => {
      const res = await testApp.fetch('/tasks');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.tasks).toBeArray();
      expect(data.total).toBeGreaterThanOrEqual(2);
    });
  });

  describe('POST /tasks', () => {
    it('should create a new task', async () => {
      const res = await testApp.fetch('/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: 'Test Task', description: 'A test task' }),
      });

      expect(res.status).toBe(201);
      const data = await res.json();
      expect(data.task.title).toBe('Test Task');
      expect(data.task.status).toBe('todo');
    });
  });

  describe('GET /tasks/:id', () => {
    it('should return a single task', async () => {
      const res = await testApp.fetch('/tasks/1');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.task.id).toBe('1');
    });
  });

  describe('PUT /tasks/:id', () => {
    it('should update a task', async () => {
      const res = await testApp.fetch('/tasks/1', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: 'done' }),
      });

      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.task.status).toBe('done');
    });
  });

  describe('DELETE /tasks/:id', () => {
    it('should delete a task', async () => {
      // Create then delete
      const createRes = await testApp.fetch('/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: 'To Delete' }),
      });
      const { task } = await createRes.json();

      const res = await testApp.fetch(`/tasks/${task.id}`, { method: 'DELETE' });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.deleted).toBe(true);
    });
  });

  describe('GET /healthz', () => {
    it('should return healthy status', async () => {
      const res = await testApp.fetch('/healthz');
      expect(res.status).toBe(200);
    });
  });
});
