import type { DatabaseService } from './database.service';
import type { RequestContextService } from './request-context.service';

// Request-scoped service with mixed dependencies
export class UserService {
  constructor(
    private db: DatabaseService,
    private ctx: RequestContextService,
  ) {}

  findById(id: string) {
    const operation = this.ctx.nextOperation();
    const result = this.db.query('SELECT * FROM users WHERE id = $1', [id]);

    return {
      user: { id, name: `User ${id}` },
      meta: {
        requestId: this.ctx.requestId,
        operation,
        totalDbQueries: this.db.getTotalQueries(),
        queryNumber: result.queryNumber,
      },
    };
  }

  listAll() {
    const operation = this.ctx.nextOperation();
    const result = this.db.query('SELECT * FROM users');

    return {
      users: [
        { id: '1', name: 'Alice' },
        { id: '2', name: 'Bob' },
        { id: '3', name: 'Charlie' },
      ],
      meta: {
        requestId: this.ctx.requestId,
        operation,
        totalDbQueries: this.db.getTotalQueries(),
        queryNumber: result.queryNumber,
      },
    };
  }
}
