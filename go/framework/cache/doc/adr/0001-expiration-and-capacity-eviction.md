# ADR 0001 — Keep expiration deadlines and capacity eviction local to each cache

- **Status**: accepted
- **Scope**: `go.putnami.dev/cache` (`go/framework/cache`)

## Decision

Every entry stores an absolute expiration instant; zero means no expiration.
Reads never return an expired value, and cleanup removes it eventually, so an
expired entry can occupy capacity until a read or sweep.

Capacity eviction is insertion-order FIFO. Updating a key neither moves it nor
evicts another key. Hit, miss, and capacity-eviction counters stay distinct.
Stored and returned byte slices are copied, so the cache never aliases a
caller's buffer.

## Rejected alternatives

- **Access-order LRU.** It mutates state on every read, with no evidence the
  cost is needed.
- **Count expiration as eviction.** Operators could not tell undersized
  capacity from normal TTL churn.
