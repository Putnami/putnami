// The data access of the bench subject, in three variants (BENCH_DB):
// - repo: @putnami/database Repository (what the 06-database sample does);
// - tx:   @putnami/database's pooled connection (useTxConnection) with a
//         hand-written query, no Repository (txa: plus abortableQuery);
// - raw:  Bun.SQL directly with its own pool, no @putnami/database at all.
import { abortableQuery, Repository, useTxConnection } from '@putnami/database';
import { SQL } from 'bun';
import { Tasks } from './tables';

export const VARIANT = (process.env.BENCH_DB ?? 'repo') as 'repo' | 'tx' | 'txa' | 'raw';
// txa: tx plus abortableQuery, as the Repository runs every statement.
const run = <T>(q: T): T => (VARIANT === 'txa' ? (abortableQuery(q as never) as T) : q);

export interface Task {
  id: string;
  title: string;
  description: string;
  done: boolean;
  priority: number;
  createdAt: string;
}

// Aligned across subjects: one toISOString per row on the driver's Date,
// never a new Date from a value already a Date.
// The Repository's entity is already the response shape (toEntity copies the
// row and turns created_at into an ISO string, DateIso): returned as it is,
// as an idiomatic Putnami service does. Its one copy matches toTask's one copy.
const fromEntity = (e: any): Task => e as Task;

// A driver row (snake_case, created_at a Date): the same function as the
// Elysia and plain-Bun subjects.
export function toTask(r: any): Task {
  return {
    id: r.id,
    title: r.title,
    description: r.description,
    done: r.done,
    priority: r.priority,
    createdAt: r.created_at.toISOString(),
  };
}

// The same through @putnami/database's client, which hands out camelCase rows.
function toTaskCamel(r: any): Task {
  return {
    id: r.id,
    title: r.title,
    description: r.description,
    done: r.done,
    priority: r.priority,
    createdAt: r.createdAt.toISOString(),
  };
}
const fromRow = VARIANT === 'raw' ? toTask : toTaskCamel;

const raw =
  VARIANT === 'raw'
    ? new SQL({
        hostname: '127.0.0.1',
        port: Number(process.env.BENCH_PG_PORT ?? 55432),
        database: 'bench',
        username: 'postgres',
        password: 'postgres',
        max: 10,
      })
    : undefined;


export async function getTask(id: string): Promise<Task | undefined> {
  if (VARIANT === 'repo') {
    const e = await new Repository(Tasks).get({ id });
    return e ? fromEntity(e) : undefined;
  }
  const sql = VARIANT === 'raw' ? (raw as SQL) : await useTxConnection('default', 'read');
  const rows = await run(sql`SELECT * FROM "tasks" WHERE "id" = ${id} LIMIT ${1}`);
  return rows.length ? fromRow(rows[0]) : undefined;
}

export async function listTasks(limit: number): Promise<Task[]> {
  if (VARIANT === 'repo') {
    const rows = await new Repository(Tasks).find({}, { limit, orderBy: 'createdAt' });
    return rows.map(fromEntity);
  }
  const sql = VARIANT === 'raw' ? (raw as SQL) : await useTxConnection('default', 'read');
  const rows = await run(sql`SELECT * FROM "tasks" ORDER BY "created_at" ASC LIMIT ${limit}`);
  return rows.map(fromRow);
}

export async function insertTask(title: string, description: string, priority: number): Promise<Task> {
  const id = crypto.randomUUID();
  if (VARIANT === 'repo') {
    const e = await new Repository(Tasks).save({ id, title, description, done: false, priority });
    return fromEntity(e);
  }
  const sql = VARIANT === 'raw' ? (raw as SQL) : await useTxConnection('default', 'write');
  const rows = await run(sql`INSERT INTO "tasks" ("id", "title", "description", "done", "priority") VALUES (${id}, ${title}, ${description}, ${false}, ${priority}) ON CONFLICT ("id") DO UPDATE SET "title" = EXCLUDED."title", "description" = EXCLUDED."description", "done" = EXCLUDED."done", "priority" = EXCLUDED."priority" RETURNING *`);
  return fromRow(rows[0]);
}
