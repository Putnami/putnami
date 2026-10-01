import { useLoaderData } from '@putnami/web';
import type { Project } from '../project.service';

export default function ProjectsPage() {
  const { projects } = useLoaderData<{ projects: Project[] }>();

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h2>Projects</h2>
        <div style={{ display: 'flex', gap: '0.75rem' }}>
          <a href='/tasks'>Tasks Board</a>
          <a href='/projects/new'>New Project</a>
        </div>
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem', marginTop: '1rem' }}>
        {projects.map((project) => (
          <div
            key={project.id}
            style={{
              padding: '1rem',
              border: '1px solid #eee',
              borderRadius: '4px',
            }}
          >
            <h3>
              <a href={`/projects/${project.id}`}>{project.name}</a>
            </h3>
            <p style={{ color: '#666' }}>{project.description}</p>
            <span
              style={{
                fontSize: '0.8rem',
                padding: '2px 8px',
                borderRadius: '4px',
                background: project.status === 'active' ? '#e6ffe6' : '#f0f0f0',
              }}
            >
              {project.status}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
