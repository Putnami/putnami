'use client';

import { CsrfInput, Form, useLoaderData, useNavigation } from '@putnami/web';
import type { Task } from '../../../store';

export default function TaskDetailPage() {
  const { task } = useLoaderData<{ task: Task }>();
  const isSubmitting = useNavigation().state === 'submitting';

  return (
    <div>
      <a href='/tasks'>Back to tasks</a>
      <h2>{task.title}</h2>
      <p>Status: {task.completed ? 'Done' : 'Pending'}</p>
      <p>Created: {new Date(task.createdAt).toLocaleString()}</p>

      <Form method='post'>
        <CsrfInput />
        <input type='hidden' name='completed' value={task.completed ? 'false' : 'true'} />
        <button type='submit' disabled={isSubmitting}>
          {isSubmitting ? 'Updating...' : task.completed ? 'Mark as Pending' : 'Mark as Done'}
        </button>
      </Form>
    </div>
  );
}
