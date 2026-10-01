// Singleton service — shared across all requests
export class DatabaseService {
  private queryCount = 0;

  // Parameterized like a real driver: values travel separately from the
  // SQL text. Never interpolate user input into SQL strings — with a
  // real database use @putnami/database's Repository / query builder.
  query(sql: string, params: unknown[] = []): { sql: string; params: unknown[]; queryNumber: number } {
    this.queryCount++;
    return { sql, params, queryNumber: this.queryCount };
  }

  getTotalQueries(): number {
    return this.queryCount;
  }
}
