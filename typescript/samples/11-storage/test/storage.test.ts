import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { HttpPlugin, application, api, http } from '@putnami/application';

describe('storage sample', () => {
  let app: Application;
  let baseUrl: string;

  beforeAll(async () => {
    app = application()
      .use(http({ port: 0 }))
      .use(api({ scanPath: 'src/api' }));
    await app.build();
    await app.start();

    const port = app.getPlugin(HttpPlugin).getServer()?.port ?? 3000;
    baseUrl = `http://localhost:${port}`;
  });

  afterAll(async () => {
    await app.stop();
  });

  it('should upload and list files', async () => {
    const uploadRes = await fetch(`${baseUrl}/files`, {
      method: 'POST',
      headers: {
        'Content-Type': 'text/plain',
        'X-File-Name': 'test.txt',
      },
      body: 'Hello, World!',
    });

    expect(uploadRes.status).toBe(201);
    const uploaded = await uploadRes.json();
    expect(uploaded.key).toBe('test.txt');
    expect(uploaded.size).toBe(13);

    // List
    const listRes = await fetch(`${baseUrl}/files`);
    expect(listRes.status).toBe(200);
    const list = await listRes.json();
    expect(list.files.length).toBeGreaterThanOrEqual(1);
  });

  it('should download a file by key', async () => {
    // Upload first
    const uploadRes = await fetch(`${baseUrl}/files`, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain', 'X-File-Name': 'download-test.txt' },
      body: 'Download me',
    });
    const { key } = await uploadRes.json();

    // Download
    const downloadRes = await fetch(`${baseUrl}/files/${key}`);
    expect(downloadRes.status).toBe(200);
    const text = await downloadRes.text();
    expect(text).toBe('Download me');
  });

  it('should delete a file', async () => {
    // Upload first
    const uploadRes = await fetch(`${baseUrl}/files`, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain', 'X-File-Name': 'to-delete.txt' },
      body: 'Delete me',
    });
    const { key } = await uploadRes.json();

    // Delete
    const deleteRes = await fetch(`${baseUrl}/files/${key}`, { method: 'DELETE' });
    expect(deleteRes.status).toBe(200);

    // Verify gone
    const getRes = await fetch(`${baseUrl}/files/${key}`);
    expect(getRes.status).toBe(404);
  });

  it('should reject invalid mime types', async () => {
    const res = await fetch(`${baseUrl}/files`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-executable' },
      body: 'bad',
    });
    expect(res.status).toBe(400);
  });
});
