'use client';

import { CsrfInput, Form, useActionData, useNavigate, useNavigation } from '@putnami/web';
import { useEffect } from 'react';

export default function NewTaskPage() {
  const actionData = useActionData<{ success: boolean }>();
  const navigation = useNavigation();
  const navigate = useNavigate();
  const isSubmitting = navigation.state === 'submitting';

  useEffect(() => {
    if (actionData?.ok) {
      navigate('/tasks');
    }
  }, [actionData, navigate]);

  return (
    <div>
      <a href='/tasks'>Back to tasks</a>
      <h2>New Task</h2>
      <Form method='post' style={{ display: 'flex', flexDirection: 'column', gap: '1rem', maxWidth: 400 }}>
        <CsrfInput />
        <div>
          <label htmlFor='title'>Title</label>
          <br />
          <input id='title' name='title' type='text' required style={{ width: '100%', padding: '0.5rem' }} />
        </div>
        <button type='submit' disabled={isSubmitting} style={{ padding: '0.5rem 1rem' }}>
          {isSubmitting ? 'Creating...' : 'Create Task'}
        </button>
        {actionData && !actionData.ok && <p style={{ color: 'red' }}>Failed to create task. Please try again.</p>}
      </Form>
    </div>
  );
}
