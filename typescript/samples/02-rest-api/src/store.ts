export interface Task {
  id: string;
  title: string;
  description: string;
  status: 'todo' | 'in_progress' | 'done';
  priority: number;
  createdAt: string;
  updatedAt: string;
}

export const tasks = new Map<string, Task>();

// Seed initial data
const now = new Date().toISOString();
tasks.set('1', {
  id: '1',
  title: 'Learn Putnami',
  description: 'Read the getting started guide',
  status: 'in_progress',
  priority: 1,
  createdAt: now,
  updatedAt: now,
});
tasks.set('2', {
  id: '2',
  title: 'Build an API',
  description: 'Create a REST API with validation',
  status: 'todo',
  priority: 2,
  createdAt: now,
  updatedAt: now,
});
