import { CsrfInput, Form, useLoaderData } from '@putnami/web';
import type { Project } from '../../project.service';
import type { Task } from '../../../tasks/task.service';

const statusColors: Record<string, string> = {
  todo: '#f0f0f0',
  in_progress: '#fff3cd',
  done: '#d4edda',
};

export default function ProjectDetailPage() {
  const { project, tasks } = useLoaderData<{ project: Project; tasks: Task[] }>();

  return (
    <div>
      <a href='/projects'>Back to projects</a>
      <h2>{project.name}</h2>
      <p style={{ color: '#666' }}>{project.description}</p>
      <div style={{ marginBottom: '1rem' }}>
        <span style={{ marginRight: '0.5rem', color: '#666' }}>Status:</span>
        <strong>{project.status}</strong>
      </div>
      <Form method='post' style={{ marginBottom: '1rem' }}>
        <CsrfInput />
        <input type='hidden' name='intent' value='toggle-project-status' />
        <button type='submit'>{project.status === 'archived' ? 'Re-open project' : 'Archive project'}</button>
      </Form>

      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: '2rem' }}>
        <h3>Tasks ({tasks.length})</h3>
      </div>

      <Form method='post' style={{ display: 'flex', gap: '0.5rem', margin: '1rem 0' }}>
        <CsrfInput />
        <input type='hidden' name='intent' value='add-task' />
        <input name='title' placeholder='New task title...' required style={{ flex: 1, padding: '0.5rem' }} />
        <button type='submit' style={{ padding: '0.5rem 1rem' }}>
          Add Task
        </button>
      </Form>

      <ul style={{ listStyle: 'none', padding: 0 }}>
        {tasks.map((task) => (
          <li
            key={task.id}
            style={{
              padding: '0.75rem',
              borderBottom: '1px solid #eee',
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
            }}
          >
            <div>
              <strong>{task.title}</strong>
              {task.assignee && <span style={{ color: '#888', marginLeft: '0.5rem' }}>({task.assignee})</span>}
            </div>
            <Form method='post' style={{ display: 'inline' }}>
              <CsrfInput />
              <input type='hidden' name='intent' value='toggle-task' />
              <input type='hidden' name='taskId' value={task.id} />
              <button
                type='submit'
                title='Click to change status'
                style={{
                  fontSize: '0.8rem',
                  padding: '2px 8px',
                  borderRadius: '4px',
                  background: statusColors[task.status] || '#f0f0f0',
                  border: '1px solid #ddd',
                  cursor: 'pointer',
                }}
              >
                {task.status.replace('_', ' ')}
              </button>
            </Form>
          </li>
        ))}
      </ul>
    </div>
  );
}
