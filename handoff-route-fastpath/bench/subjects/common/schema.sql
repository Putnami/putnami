DROP TABLE IF EXISTS tasks;
CREATE TABLE tasks (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  done BOOLEAN NOT NULL DEFAULT false,
  priority INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX tasks_created_at ON tasks (created_at, id);
INSERT INTO tasks (id, title, description, priority, created_at)
SELECT ('00000000-0000-4000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
       'Task ' || i, 'Description of task ' || i, (i % 5) + 1,
       timestamptz '2026-01-01' + i * interval '1 minute'
FROM generate_series(1, 1000) AS i;
