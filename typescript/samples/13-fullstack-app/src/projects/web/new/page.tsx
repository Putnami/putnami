import { useEffect } from 'react';
import { CsrfInput, Form, useActionData, useNavigate } from '@putnami/web';
import type { Project } from '../../project.service';

export default function NewProjectPage() {
  const result = useActionData<{ project: Project }>();
  const navigate = useNavigate();

  useEffect(() => {
    if (result?.ok) {
      navigate(`/projects/${result.project.id}`);
    }
  }, [result, navigate]);

  return (
    <div>
      <a href='/projects'>Back to projects</a>
      <h2>New Project</h2>
      {result && !result.ok && (
        <p style={{ color: '#c00', padding: '0.5rem', background: '#fff0f0', borderRadius: '4px' }}>
          Failed to create project. Please check your input and try again.
        </p>
      )}
      <Form method='post' style={{ display: 'flex', flexDirection: 'column', gap: '1rem', maxWidth: 500 }}>
        <CsrfInput />
        <div>
          <label htmlFor='name'>Name</label>
          <br />
          <input id='name' name='name' type='text' required style={{ width: '100%', padding: '0.5rem' }} />
        </div>
        <div>
          <label htmlFor='description'>Description</label>
          <br />
          <textarea id='description' name='description' rows={3} style={{ width: '100%', padding: '0.5rem' }} />
        </div>
        <button type='submit' style={{ padding: '0.5rem 1rem', alignSelf: 'flex-start' }}>
          Create Project
        </button>
      </Form>
    </div>
  );
}
