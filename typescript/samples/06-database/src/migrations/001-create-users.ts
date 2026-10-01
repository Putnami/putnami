export const migration = {
  name: '20260211194151_create_users',
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL,
      age INTEGER,
      created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
    );

    CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
  `,
  down: `
    DROP INDEX IF EXISTS idx_users_email;
    DROP TABLE IF EXISTS users;
  `,
};
