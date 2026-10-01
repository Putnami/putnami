import { topic, Uuid } from '@putnami/events';

export const TaskCreated = topic('task.created', {
  taskId: Uuid,
  projectId: Uuid,
  title: String,
  status: String,
  priority: Number,
  source: String,
});

export const TaskStatusChanged = topic('task.status.changed', {
  taskId: Uuid,
  projectId: Uuid,
  status: String,
  totalTasks: Number,
  remainingOpenTasks: Number,
});
