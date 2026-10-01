// Request-scoped service — new instance per request
export class RequestContextService {
  readonly requestId = crypto.randomUUID();
  readonly startTime = Date.now();
  private operationCount = 0;

  nextOperation(): number {
    return ++this.operationCount;
  }

  getElapsedMs(): number {
    return Date.now() - this.startTime;
  }
}
