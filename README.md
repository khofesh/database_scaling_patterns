# Database Scaling Patterns

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18

## Projects

- **[simulate_database_partitioning](./simulate_database_partitioning)** - Demonstrates PostgreSQL table partitioning strategies
- **[simulate_database_sharding](./simulate_database_sharding)** - Demonstrates database sharding across multiple PostgreSQL instances
- **[simulate_read_replicas](./simulate_read_replicas)** - Primary + streaming replicas: read scaling, replication lag, read-your-writes
- **[simulate_connection_pooling](./simulate_connection_pooling)** - PgBouncer multiplexing many clients onto a few PostgreSQL connections
- **[simulate_query_caching](./simulate_query_caching)** - Redis cache-aside with TTL, write-through, and invalidation
- **[simulate_read_write_splitting](./simulate_read_write_splitting)** - Application-level router sending writes to the primary and reads to replicas
