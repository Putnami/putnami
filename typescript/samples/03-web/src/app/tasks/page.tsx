import { useLoaderData } from '@putnami/web';
import type { Task } from '../../store';

export default function TasksPage() {
  const { tasks } = useLoaderData<{ tasks: Task[] }>();

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h2>Tasks</h2>
        <a href='/tasks/new'>Add Task</a>
      </div>
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
            <span style={{ textDecoration: task.completed ? 'line-through' : 'none' }}>
              <a href={`/tasks/${task.id}`}>{task.title}</a>
            </span>
            <span style={{ color: task.completed ? 'green' : 'gray', fontSize: '0.85rem' }}>
              {task.completed ? 'Done' : 'Pending'}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
