import { getPublisher } from '@putnami/events';
import { Repository } from '@putnami/database';
import { publishActivity } from '../shared/activity.feed';
import { Projects, type Project } from './projects.entity';
import { ProjectCreated, ProjectStatusChanged } from './projects.topics';

class ProjectRepository extends Repository<typeof Projects> {
  constructor() {
    super(Projects);
  }
}

export type { Project };

const publishProjectCreated = getPublisher(ProjectCreated);
const publishProjectStatusChanged = getPublisher(ProjectStatusChanged);

export class ProjectService {
  private repo = new ProjectRepository();

  async listProjects(): Promise<Project[]> {
    return this.repo.find({}, { orderBy: 'createdAt DESC' });
  }

  async getProject(id: string): Promise<Project | undefined> {
    return this.repo.get({ id });
  }

  async createProject(data: { name: string; description: string }): Promise<Project> {
    const project = await this.repo.save(
      {
        id: crypto.randomUUID(),
        name: data.name,
        description: data.description,
        status: 'active',
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      },
      { strict: true },
    );

    await publishProjectCreated({
      projectId: project.id,
      name: project.name,
      description: project.description,
      status: project.status,
    });

    publishActivity('projects', {
      type: 'project.created',
      message: `Project "${project.name}" created`,
      projectId: project.id,
    });

    return project;
  }

  async setStatus(projectId: string, status: string, reason: string): Promise<Project | undefined> {
    const project = await this.repo.get({ id: projectId });
    if (!project) return undefined;
    if (project.status === status) return project;

    const updated = await this.repo.save({
      ...project,
      status,
      createdAt: String(project.createdAt),
      updatedAt: new Date().toISOString(),
    });

    await publishProjectStatusChanged({
      projectId: updated.id,
      status: updated.status,
      reason,
    });

    publishActivity('projects', {
      type: 'project.status.changed',
      message: `Project "${updated.name}" is now ${updated.status}`,
      projectId: updated.id,
    });

    return updated;
  }
}
