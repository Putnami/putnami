import { CsrfInput, Form, useLoaderData } from '@putnami/web';
import type { Task } from '../task.service';

const statusColor: Record<string, string> = {
  todo: '#f0f0f0',
  in_progress: '#fff3cd',
  done: '#d4edda',
};

export default function TasksBoardPage() {
  const { tasks, projectNames } = useLoaderData<{ tasks: Task[]; projectNames: Record<string, string> }>();

  return (
    <div>
      <h2>Tasks Board</h2>
      <p style={{ color: '#666' }}>Tasks module web app view. Toggle status directly from here.</p>

      <ul style={{ listStyle: 'none', padding: 0, marginTop: '1rem' }}>
        {tasks.map((task) => (
          <li
            key={task.id}
            style={{
              border: '1px solid #eee',
              borderRadius: '8px',
              padding: '0.85rem',
              marginBottom: '0.75rem',
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
            }}
          >
            <div>
              <strong>{task.title}</strong>
              <div style={{ color: '#666', fontSize: '0.9rem' }}>
                Project: <a href={`/projects/${task.projectId}`}>{projectNames[task.projectId] ?? task.projectId}</a>
              </div>
            </div>

            <Form method='post'>
              <CsrfInput />
              <input type='hidden' name='taskId' value={task.id} />
              <button
                type='submit'
                style={{
                  border: '1px solid #ddd',
                  borderRadius: '6px',
                  background: statusColor[task.status] ?? '#f0f0f0',
                  padding: '0.35rem 0.65rem',
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
