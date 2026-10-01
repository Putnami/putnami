# Advanced Queries

The Repository covers CRUD and filtering. For JOINs, subqueries, CTEs, and aggregations, use postgres.js directly through `database()` or `this.conn()`. This guide shows how to write these queries safely and idiomatically within the `@putnami/database` ecosystem.

## Table of contents

- [When to use raw SQL](#when-to-use-raw-sql)
- [Getting a connection](#getting-a-connection)
- [JOINs](#joins)
- [Subqueries](#subqueries)
- [Common Table Expressions (CTEs)](#common-table-expressions-ctes)
- [Aggregations](#aggregations)
- [Mixing raw SQL with transactions](#mixing-raw-sql-with-transactions)
- [Abort-safe raw queries](#abort-safe-raw-queries)
- [Typing results](#typing-results)
- [Performance](#performance)

## When to use raw SQL

| Use case | Approach |
|---|---|
| CRUD on a single table | `Repository` methods (`find`, `save`, `delete`) |
| Filtering with operators | `QueryFilters` (`$or`, `$and`, `IN`, `LIKE`, etc.) |
| JOINs across tables | Raw SQL via `this.conn()` or `database()` |
| Aggregations (COUNT, SUM, AVG) | Raw SQL |
| Subqueries, EXISTS, IN (select) | Raw SQL |
| CTEs / recursive queries | Raw SQL |
| Window functions | Raw SQL |

The rule of thumb: if it touches more than one table or uses SQL features beyond WHERE/ORDER BY/LIMIT, write raw SQL.

## Getting a connection

**Inside a custom repository method** — use `this.conn()`. It is transaction-aware and abort-checked:

```ts
class OrderRepository extends Repository<typeof OrdersTable> {
  constructor() {
    super(OrdersTable);
  }

  async findWithItems(orderId: string) {
    const sql = await this.conn('read');
    return sql`
      SELECT o.*, i.product_id, i.quantity, i.price
      FROM orders o
      INNER JOIN order_items i ON i.order_id = o.id
      WHERE o.id = ${orderId}
    `;
  }
}
```

**Outside a repository** — use `useTxConnection()` (transaction-aware, abort-checked) or `database()` (pool-level, no transaction):

```ts
import { useTxConnection, database } from '@putnami/database';

// Transaction-aware — participates in runInTransaction if active
const sql = await useTxConnection(undefined, 'read');

// Pool-level — always gets a fresh pool connection, ignores transactions
const sql = await database();
```

Use `useTxConnection()` when your code might run inside a transaction. Use `database()` for standalone scripts, migrations, or health checks where you explicitly want to bypass the transaction context.

## JOINs

### INNER JOIN

Return only rows that match in both tables:

```ts
async findUsersWithOrders() {
  const sql = await this.conn('read');
  return sql`
    SELECT u.id, u.name, u.email, o.id AS order_id, o.total
    FROM users u
    INNER JOIN orders o ON o.user_id = u.id
    WHERE o.status = ${'completed'}
    ORDER BY o.total DESC
  `;
}
```

### LEFT JOIN

Return all users, with order data when it exists:

```ts
async findUsersWithOrderCount() {
  const sql = await this.conn('read');
  return sql`
    SELECT u.id, u.name, COUNT(o.id) AS order_count
    FROM users u
    LEFT JOIN orders o ON o.user_id = u.id
    GROUP BY u.id, u.name
    ORDER BY order_count DESC
  `;
}
```

### Multiple JOINs

```ts
async findOrderDetails(orderId: string) {
  const sql = await this.conn('read');
  return sql`
    SELECT
      o.id,
      o.created_at,
      u.name AS customer_name,
      u.email AS customer_email,
      p.name AS product_name,
      i.quantity,
      i.price,
      (i.quantity * i.price) AS line_total
    FROM orders o
    INNER JOIN users u ON u.id = o.user_id
    INNER JOIN order_items i ON i.order_id = o.id
    INNER JOIN products p ON p.id = i.product_id
    WHERE o.id = ${orderId}
    ORDER BY p.name
  `;
}
```

### Self JOIN

```ts
async findEmployeesWithManagers() {
  const sql = await this.conn('read');
  return sql`
    SELECT
      e.id,
      e.name AS employee,
      m.name AS manager
    FROM employees e
    LEFT JOIN employees m ON m.id = e.manager_id
    ORDER BY m.name, e.name
  `;
}
```

## Subqueries

### IN with subquery

```ts
async findUsersWithRecentOrders(since: string) {
  const sql = await this.conn('read');
  return sql`
    SELECT *
    FROM users
    WHERE id IN (
      SELECT DISTINCT user_id
      FROM orders
      WHERE created_at >= ${since}
    )
  `;
}
```

### EXISTS

More efficient than `IN` when the subquery could return many rows:

```ts
async findUsersWhoOrdered(productId: string) {
  const sql = await this.conn('read');
  return sql`
    SELECT u.*
    FROM users u
    WHERE EXISTS (
      SELECT 1
      FROM orders o
      INNER JOIN order_items i ON i.order_id = o.id
      WHERE o.user_id = u.id
        AND i.product_id = ${productId}
    )
  `;
}
```

### NOT EXISTS

```ts
async findUsersWithoutOrders() {
  const sql = await this.conn('read');
  return sql`
    SELECT u.*
    FROM users u
    WHERE NOT EXISTS (
      SELECT 1 FROM orders o WHERE o.user_id = u.id
    )
  `;
}
```

### Scalar subquery

```ts
async findUsersWithLatestOrder() {
  const sql = await this.conn('read');
  return sql`
    SELECT
      u.*,
      (
        SELECT MAX(o.created_at)
        FROM orders o
        WHERE o.user_id = u.id
      ) AS last_order_at
    FROM users u
    ORDER BY last_order_at DESC NULLS LAST
  `;
}
```

## Common Table Expressions (CTEs)

CTEs improve readability for multi-step queries and can be referenced multiple times.

### Basic CTE

```ts
async findTopCustomers(minSpent: number) {
  const sql = await this.conn('read');
  return sql`
    WITH customer_totals AS (
      SELECT
        user_id,
        SUM(total) AS total_spent,
        COUNT(*) AS order_count
      FROM orders
      WHERE status = 'completed'
      GROUP BY user_id
    )
    SELECT u.id, u.name, u.email, ct.total_spent, ct.order_count
    FROM users u
    INNER JOIN customer_totals ct ON ct.user_id = u.id
    WHERE ct.total_spent >= ${minSpent}
    ORDER BY ct.total_spent DESC
  `;
}
```

### Multiple CTEs

```ts
async findProductPerformance() {
  const sql = await this.conn('read');
  return sql`
    WITH product_sales AS (
      SELECT
        i.product_id,
        SUM(i.quantity) AS units_sold,
        SUM(i.quantity * i.price) AS revenue
      FROM order_items i
      INNER JOIN orders o ON o.id = i.order_id
      WHERE o.status = 'completed'
      GROUP BY i.product_id
    ),
    product_returns AS (
      SELECT
        product_id,
        COUNT(*) AS return_count
      FROM returns
      GROUP BY product_id
    )
    SELECT
      p.name,
      COALESCE(ps.units_sold, 0) AS units_sold,
      COALESCE(ps.revenue, 0) AS revenue,
      COALESCE(pr.return_count, 0) AS returns,
      CASE
        WHEN ps.units_sold > 0
        THEN ROUND(pr.return_count::numeric / ps.units_sold * 100, 2)
        ELSE 0
      END AS return_rate
    FROM products p
    LEFT JOIN product_sales ps ON ps.product_id = p.id
    LEFT JOIN product_returns pr ON pr.product_id = p.id
    ORDER BY revenue DESC
  `;
}
```

### Recursive CTE

Useful for hierarchical data (categories, org charts, threaded comments):

```ts
async findCategoryTree(rootId: string) {
  const sql = await this.conn('read');
  return sql`
    WITH RECURSIVE category_tree AS (
      -- Base case: the root category
      SELECT id, name, parent_id, 0 AS depth
      FROM categories
      WHERE id = ${rootId}

      UNION ALL

      -- Recursive case: children
      SELECT c.id, c.name, c.parent_id, ct.depth + 1
      FROM categories c
      INNER JOIN category_tree ct ON ct.id = c.parent_id
    )
    SELECT * FROM category_tree
    ORDER BY depth, name
  `;
}
```

## Aggregations

### COUNT, SUM, AVG

```ts
async getOrderStats() {
  const sql = await this.conn('read');
  const rows = await sql`
    SELECT
      COUNT(*) AS total_orders,
      COUNT(*) FILTER (WHERE status = 'completed') AS completed,
      COUNT(*) FILTER (WHERE status = 'pending') AS pending,
      COALESCE(SUM(total), 0) AS revenue,
      COALESCE(AVG(total), 0) AS avg_order_value
    FROM orders
  `;
  return rows[0];
}
```

### GROUP BY with HAVING

```ts
async findFrequentBuyers(minOrders: number) {
  const sql = await this.conn('read');
  return sql`
    SELECT
      u.id,
      u.name,
      COUNT(o.id) AS order_count,
      SUM(o.total) AS total_spent
    FROM users u
    INNER JOIN orders o ON o.user_id = u.id
    GROUP BY u.id, u.name
    HAVING COUNT(o.id) >= ${minOrders}
    ORDER BY total_spent DESC
  `;
}
```

### Window functions

```ts
async findOrdersWithRunningTotal(userId: string) {
  const sql = await this.conn('read');
  return sql`
    SELECT
      id,
      total,
      created_at,
      SUM(total) OVER (ORDER BY created_at) AS running_total,
      ROW_NUMBER() OVER (ORDER BY created_at) AS order_number
    FROM orders
    WHERE user_id = ${userId}
    ORDER BY created_at
  `;
}
```

## Mixing raw SQL with transactions

Raw queries through `this.conn()` or `useTxConnection()` participate in the current transaction automatically. No extra work needed:

```ts
import { runInTransaction } from '@putnami/database';

await runInTransaction(async () => {
  // Repository call — uses the transaction connection
  await orderRepo.save({ id: orderId, status: 'shipped' });

  // Raw SQL — same transaction connection
  const sql = await orderRepo.conn();
  await sql`
    INSERT INTO audit_log (entity, entity_id, action, created_at)
    VALUES (${'order'}, ${orderId}, ${'shipped'}, NOW())
  `;

  // Another raw query — still the same transaction
  await sql`
    UPDATE inventory
    SET reserved = reserved - 1
    WHERE product_id IN (
      SELECT product_id FROM order_items WHERE order_id = ${orderId}
    )
  `;
});
// All three operations commit together or roll back together
```

This works because `this.conn()` calls `useTxConnection()` internally, which returns the reserved transaction connection when one is active.

## Abort-safe raw queries

When writing raw queries outside the Repository, wrap them with `abortableQuery` so they get cancelled if the HTTP request is aborted:

```ts
import { abortableQuery, useTxConnection } from '@putnami/database';

async function heavyReport(filters: ReportFilters) {
  const sql = await useTxConnection(undefined, 'read');

  const rows = await abortableQuery(sql`
    SELECT
      date_trunc('month', created_at) AS month,
      COUNT(*) AS orders,
      SUM(total) AS revenue
    FROM orders
    WHERE created_at >= ${filters.since}
    GROUP BY month
    ORDER BY month
  `);

  return rows;
}
```

If the client disconnects while this query runs, `abortableQuery` sends a PostgreSQL cancel request and throws `QueryAbortError`. Without it, the query runs to completion on the server even though nobody is waiting for the result.

Repository methods already use `abortableQuery` internally — you only need it for raw queries.

## Typing results

Queries return `SqlResult<SqlRow[]>` by default (putnami's aliases of postgres.js's `RowList`/`Row`). For type safety, define an interface for your result shape:

```ts
interface UserWithStats {
  id: string;
  name: string;
  email: string;
  order_count: number;
  total_spent: number;
}

async findUsersWithStats(): Promise<UserWithStats[]> {
  const sql = await this.conn('read');
  const rows = await sql`
    SELECT u.id, u.name, u.email,
           COUNT(o.id)::int AS order_count,
           COALESCE(SUM(o.total), 0)::numeric AS total_spent
    FROM users u
    LEFT JOIN orders o ON o.user_id = u.id
    GROUP BY u.id
    ORDER BY total_spent DESC
  `;
  return rows as unknown as UserWithStats[];
}
```

For queries that return entities from a single table, use `helper.toEntity()` to map rows back to the inferred type:

```ts
async findActiveByJoin(): Promise<User[]> {
  const sql = await this.conn('read');
  const rows = await sql`
    SELECT u.*
    FROM users u
    INNER JOIN subscriptions s ON s.user_id = u.id
    WHERE s.status = 'active'
  `;
  return rows.map(row => this.helper.toEntity<User>(row));
}
```

## Performance

### Use EXPLAIN to understand query plans

```ts
async explainQuery() {
  const sql = await this.conn('read');
  const plan = await sql`
    EXPLAIN ANALYZE
    SELECT u.*, COUNT(o.id) AS order_count
    FROM users u
    LEFT JOIN orders o ON o.user_id = u.id
    GROUP BY u.id
  `;
  for (const row of plan) {
    console.log(row['QUERY PLAN']);
  }
}
```

### Index the columns you JOIN and filter on

JOINs are only fast when the joined column is indexed. Foreign keys don't create indexes automatically in PostgreSQL:

```sql
-- Migration: add indexes for JOIN performance
CREATE INDEX idx_orders_user_id ON orders (user_id);
CREATE INDEX idx_order_items_order_id ON order_items (order_id);
CREATE INDEX idx_order_items_product_id ON order_items (product_id);
```

### Avoid N+1 queries

Instead of looping and querying per item:

```ts
// Bad: N+1 queries
const users = await userRepo.find({ status: 'active' });
for (const user of users) {
  const orders = await orderRepo.find({ userId: user.id });
  user.orders = orders;
}
```

Use a single JOIN or a batched IN query:

```ts
// Good: single query
async findUsersWithOrders() {
  const sql = await this.conn('read');
  const rows = await sql`
    SELECT
      u.id, u.name,
      COALESCE(
        json_agg(json_build_object('id', o.id, 'total', o.total))
        FILTER (WHERE o.id IS NOT NULL),
        '[]'
      ) AS orders
    FROM users u
    LEFT JOIN orders o ON o.user_id = u.id
    WHERE u.status = 'active'
    GROUP BY u.id, u.name
  `;
  return rows;
}
```

### Paginate with LIMIT/OFFSET or cursors

Always use LIMIT. The Repository defaults to 1000 rows — raw queries don't have this safety net:

```ts
// Always set a LIMIT on raw queries
const rows = await sql`
  SELECT * FROM large_table
  WHERE status = ${status}
  ORDER BY created_at DESC
  LIMIT ${pageSize}
  OFFSET ${(page - 1) * pageSize}
`;
```

For large offsets (> 10k rows), consider cursor-based pagination:

```ts
// Cursor-based: faster for deep pages
const rows = await sql`
  SELECT * FROM large_table
  WHERE created_at < ${cursor}
    AND status = ${status}
  ORDER BY created_at DESC
  LIMIT ${pageSize}
`;
```
