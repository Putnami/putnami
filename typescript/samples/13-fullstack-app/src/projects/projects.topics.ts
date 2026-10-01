import { topic, Uuid } from '@putnami/events';

export const ProjectCreated = topic('project.created', {
  projectId: Uuid,
  name: String,
  description: String,
  status: String,
});

export const ProjectStatusChanged = topic('project.status.changed', {
  projectId: Uuid,
  status: String,
  reason: String,
});
