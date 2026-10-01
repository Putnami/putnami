export interface Task {
  id: string;
  title: string;
  completed: boolean;
  createdAt: string;
}

export const tasks = new Map<string, Task>();

// Seed data
const now = new Date().toISOString();
tasks.set('1', { id: '1', title: 'Learn Putnami React', completed: false, createdAt: now });
tasks.set('2', { id: '2', title: 'Build a fullstack app', completed: false, createdAt: now });
tasks.set('3', { id: '3', title: 'Deploy to production', completed: false, createdAt: now });
