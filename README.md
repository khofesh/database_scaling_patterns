# Database Scaling Patterns

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (and CockroachDB v26.2 for the NewSQL example)

## Projects

- **[simulate_database_partitioning](./simulate_database_partitioning)** - Demonstrates PostgreSQL table partitioning strategies
- **[simulate_database_sharding](./simulate_database_sharding)** - Demonstrates database sharding across multiple PostgreSQL instances
- **[simulate_read_replicas](./simulate_read_replicas)** - Primary + streaming replicas: read scaling, replication lag, read-your-writes
- **[simulate_connection_pooling](./simulate_connection_pooling)** - PgBouncer multiplexing many clients onto a few PostgreSQL connections
- **[simulate_query_caching](./simulate_query_caching)** - Redis cache-aside with TTL, write-through, and invalidation
- **[simulate_read_write_splitting](./simulate_read_write_splitting)** - Application-level router sending writes to the primary and reads to replicas
- **[simulate_consistent_hashing](./simulate_consistent_hashing)** - Hash ring with virtual nodes: minimal data movement on resize vs modulo sharding
- **[simulate_cqrs](./simulate_cqrs)** - Command Query Responsibility Segregation: normalized write store + denormalized read store via a projector
- **[simulate_automatic_failover](./simulate_automatic_failover)** - Health-checked failover manager promoting a standby with pg_promote()
- **[simulate_multitenancy_shared_schema](./simulate_multitenancy_shared_schema)** - Shared schema multi-tenancy isolated with PostgreSQL Row-Level Security
- **[simulate_event_sourcing](./simulate_event_sourcing)** - Append-only event log: state reconstruction by replay, snapshots, optimistic concurrency
- **[simulate_newsql](./simulate_newsql)** - 3-node CockroachDB cluster: distributed ACID transactions, auto-sharding/replication, fault tolerance
- **[simulate_data_tiering](./simulate_data_tiering)** - Hot/warm/cold storage tiers via partitioning + tablespaces with age-based migration
