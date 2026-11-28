import { SQL } from 'bun';
import { toTask } from './protect';

// The same pool size (10) and the same statements as Putnami's Repository.
export const sql = new SQL({
  hostname: '127.0.0.1',
  port: Number(process.env.BENCH_PG_PORT ?? 55432),
  database: 'bench',
  username: 'postgres',
  password: 'postgres',
  max: 10,
});

export async function getTask(id: string) {
  const rows = await sql`SELECT * FROM "tasks" WHERE "id" = ${id} LIMIT ${1}`;
  return rows.length ? toTask(rows[0]) : undefined;
}

export async function listTasks(limit: number) {
  const rows = await sql`SELECT * FROM "tasks" ORDER BY "created_at" ASC LIMIT ${limit}`;
  return rows.map(toTask);
}

export async function insertTask(title: string, description: string, priority: number) {
  const id = crypto.randomUUID();
  const rows = await sql`INSERT INTO "tasks" ("id", "title", "description", "done", "priority") VALUES (${id}, ${title}, ${description}, ${false}, ${priority}) ON CONFLICT ("id") DO UPDATE SET "title" = EXCLUDED."title", "description" = EXCLUDED."description", "done" = EXCLUDED."done", "priority" = EXCLUDED."priority" RETURNING *`;
  return toTask(rows[0]);
}
