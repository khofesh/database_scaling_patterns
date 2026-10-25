# Database Scaling Patterns - TODO

This document tracks database scaling methods and patterns to implement in this project.

## Completed

- [x] **Table Partitioning** - `simulate_database_partitioning/`
  - Range partitioning, list partitioning, hash partitioning within a single database
- [x] **Database Sharding** - `simulate_database_sharding/`
  - Horizontal sharding across multiple database instances with application-level routing
- [x] **Read Replicas** - `simulate_read_replicas/`
  - Primary + 2 streaming replicas, write to primary / read from replicas, load balancing, replication lag, read-your-writes
- [x] **Connection Pooling** - `simulate_connection_pooling/`
  - PgBouncer transaction pooling, connection exhaustion vs multiplexing, pool sizing, `SHOW POOLS`
- [x] **Database Query Caching** - `simulate_query_caching/`
  - Redis cache-aside, TTL, write-through, write+invalidate, hit ratio, graceful degradation
- [x] **Read-Write Splitting** - `simulate_read_write_splitting/`
  - Application-level router (writes→primary, reads→replicas) with LSN-based read-your-writes
- [x] **Consistent Hashing** - `simulate_consistent_hashing/`
  - Hash ring with virtual nodes, minimal data movement on node add/remove, comparison vs modulo sharding
- [x] **CQRS** - `simulate_cqrs/`
  - Normalized write store + denormalized read store, transactional outbox, projector, eventual consistency
- [x] **Automatic Failover** - `simulate_automatic_failover/`
  - Health-checked failover manager, `pg_promote()` of a hot standby, write redirection, split-brain notes
- [x] **Multi-Tenancy (Shared Schema)** - `simulate_multitenancy_shared_schema/`
  - Single schema + `tenant_id` column isolated with Row-Level Security (USING/WITH CHECK), fail-closed
- [x] **Event Sourcing** - `simulate_event_sourcing/`
  - Append-only event log, state reconstruction by replay, snapshots, optimistic concurrency via version
- [x] **NewSQL (CockroachDB)** - `simulate_newsql/`
  - 3-node cluster, distributed serializable ACID transactions, automatic sharding/replication, fault tolerance
- [x] **Data Tiering** - `simulate_data_tiering/`
  - Hot/warm/cold tablespaces + monthly partitions, age-based migration, partition pruning, transparent queries
- [x] **Multi-Primary Replication** - `simulate_multi_primary_replication/`
  - Bidirectional logical replication (`origin = none`), per-node key ranges, divergence, PG18 conflict stats, last-writer-wins trigger
- [x] **Synchronous vs Asynchronous Replication** - `simulate_sync_async_replication/`
  - `synchronous_commit` levels, stale reads, blocked commits, quorum `ANY n` commit
- [x] **Materialized Views** - `simulate_materialized_views/`
  - On-demand vs `CONCURRENTLY` refresh, periodic refresh, incremental summary with MERGE + watermark
- [x] **Connection Load Balancing** - `simulate_connection_load_balancing/`
  - HAProxy write/read endpoints, `pgsql-check` health checks, backup server, per-connection balancing
- [x] **Directory-Based Sharding** - `simulate_directory_based_sharding/`
  - Lookup service + cache, least-loaded placement, tenant move, stale-cache fencing, vs hash routing
- [x] **Range-Based Sharding** - `simulate_range_based_sharding/`
  - Date ranges, range-query pruning, write hotspot, range split, hash-prefixed current range
- [x] **Vertical Partitioning** - `simulate_vertical_partitioning/`
  - Hot/cold/BLOB split, inline vs TOAST, pages read, `SELECT *` bytes, WAL per update
- [x] **Database Denormalization** - `simulate_denormalization/`
  - Redundant columns via statement-level triggers (transition tables), fan-out cost, drift + reconciliation
- [x] **Federation / Functional Partitioning** - `simulate_federation/`
  - Database per domain, app-side joins, `postgres_fdw` pushdown, orphaned references
- [x] **Hot Standby** - `simulate_hot_standby/`
  - Hot (streaming) vs warm (log shipping) standby, WAL archiving, base backup, PITR to a restore point
- [x] **Database Indexing Strategies** - `simulate_indexing_strategies/`
  - B-tree, Hash, GIN, GiST (exclusion constraint), BRIN, partial, covering, expression; bloat, unused indexes
- [x] **Data Archival** - `simulate_data_archival/`
  - Batched `DELETE … RETURNING` mover, cold-tuned archive table, retrieval strategies, gzip export
- [x] **Multi-Tenancy (Separate Schema)** - `simulate_multitenancy_separate_schema/`
  - Schema + role per tenant, transaction-scoped routing, per-schema migrations, catalog growth
- [x] **Multi-Tenancy (Separate Database)** - `simulate_multitenancy_separate_database/`
  - Database + role per tenant, `CONNECTION LIMIT`, per-DB settings, tenant moves, connection explosion
- [x] **Polyglot Persistence** - `simulate_polyglot_persistence/`
  - PostgreSQL + Redis + Elasticsearch synced via transactional outbox and idempotent relay

---

## Pattern Catalogue (all implemented)

### Replication Patterns

- [x] **Multi-Primary Replication** - `simulate_multi_primary_replication/`
  - Multiple writable nodes
  - Conflict resolution strategies
  - Use cases: geographic distribution, high availability

- [x] **Synchronous vs Asynchronous Replication** - `simulate_sync_async_replication/`
  - Trade-offs between consistency and performance
  - Quorum-based replication

### Caching Patterns

- [x] **Materialized Views** - `simulate_materialized_views/`
  - Pre-computed query results
  - Refresh strategies (on-demand, periodic, incremental)

### Connection Management

- [x] **Connection Load Balancing** - `simulate_connection_load_balancing/`
  - HAProxy / pgpool for distributing connections
  - Health checks and failover

### Data Distribution Patterns

- [x] **Consistent Hashing** - `simulate_consistent_hashing/`
  - Virtual nodes for better distribution
  - Minimal data movement on node changes
  - Comparison with modulo-based sharding

- [x] **Directory-Based Sharding** - `simulate_directory_based_sharding/`
  - Lookup service for shard mapping
  - Dynamic shard assignment
  - Trade-offs vs hash-based routing

- [x] **Range-Based Sharding** - `simulate_range_based_sharding/`
  - Sharding by date ranges, geographic regions
  - Handling hotspots and rebalancing

### Vertical Scaling Techniques

- [x] **Vertical Partitioning** - `simulate_vertical_partitioning/`
  - Splitting tables by columns
  - Separating hot and cold data
  - BLOB/CLOB storage separation

- [x] **Database Denormalization** - `simulate_denormalization/`
  - Strategic redundancy for read performance
  - Maintaining consistency with triggers/application logic

### Distributed Database Patterns

- [x] **Federation / Functional Partitioning** - `simulate_federation/`
  - Separate databases by domain (users DB, orders DB, analytics DB)
  - Cross-database queries and joins

- [x] **CQRS (Command Query Responsibility Segregation)** - `simulate_cqrs/`
  - Separate read and write models
  - Event sourcing integration
  - Eventual consistency handling

- [x] **Event Sourcing** - `simulate_event_sourcing/`
  - Append-only event log
  - State reconstruction from events
  - Snapshots for performance

### High Availability Patterns

- [x] **Automatic Failover** - `simulate_automatic_failover/`
  - Patroni / repmgr for PostgreSQL HA
  - Leader election mechanisms
  - Split-brain prevention

- [x] **Hot Standby** - `simulate_hot_standby/`
  - Warm standby vs hot standby
  - Streaming replication setup
  - Point-in-time recovery (PITR)

### Query Optimization Patterns

- [x] **Database Indexing Strategies** - `simulate_indexing_strategies/`
  - B-tree, Hash, GIN, GiST indexes
  - Partial indexes, covering indexes
  - Index maintenance and bloat

### Data Archival & Tiering

- [x] **Data Archival** - `simulate_data_archival/`
  - Moving old data to cheaper storage
  - Archive tables with different storage parameters
  - Retrieval strategies

- [x] **Hot/Warm/Cold Data Tiering** - `simulate_data_tiering/`
  - Automatic data movement based on access patterns
  - Tablespace management
  - Integration with object storage (S3)

### Multi-Tenancy Patterns

- [x] **Shared Database, Shared Schema** - `simulate_multitenancy_shared_schema/`
  - Tenant ID column approach
  - Row-level security (RLS)

- [x] **Shared Database, Separate Schema** - `simulate_multitenancy_separate_schema/`
  - Schema per tenant
  - Connection routing

- [x] **Separate Database per Tenant** - `simulate_multitenancy_separate_database/`
  - Full isolation
  - Resource management challenges

### Emerging Patterns

- [x] **NewSQL Databases** - `simulate_newsql/`
  - CockroachDB, TiDB, YugabyteDB
  - Distributed SQL with ACID guarantees
  - Comparison with traditional sharding

- [x] **Polyglot Persistence** - `simulate_polyglot_persistence/`
  - Using multiple database types
  - PostgreSQL + Redis + Elasticsearch
  - Data synchronization strategies

---

## Priority Suggestions

### High Priority (Common patterns) — ✅ done

1. ~~Read Replicas~~ ✅
2. ~~Connection Pooling~~ ✅
3. ~~Database Query Caching~~ ✅
4. ~~Read-Write Splitting~~ ✅

### Medium Priority (Advanced scaling) — ✅ done

5. ~~Consistent Hashing~~ ✅
6. ~~CQRS~~ ✅
7. ~~Automatic Failover~~ ✅
8. ~~Multi-Tenancy (Shared Schema)~~ ✅

### Lower Priority (Specialized use cases) — ✅ done

9. ~~Event Sourcing~~ ✅
10. ~~NewSQL exploration~~ ✅
11. ~~Data Tiering~~ ✅

### Remaining catalogue — ✅ done

12. ~~Multi-Primary Replication~~ ✅
13. ~~Synchronous vs Asynchronous Replication~~ ✅
14. ~~Materialized Views~~ ✅
15. ~~Connection Load Balancing~~ ✅
16. ~~Directory-Based Sharding~~ ✅
17. ~~Range-Based Sharding~~ ✅
18. ~~Vertical Partitioning~~ ✅
19. ~~Database Denormalization~~ ✅
20. ~~Federation / Functional Partitioning~~ ✅
21. ~~Hot Standby~~ ✅
22. ~~Database Indexing Strategies~~ ✅
23. ~~Data Archival~~ ✅
24. ~~Multi-Tenancy (Separate Schema)~~ ✅
25. ~~Multi-Tenancy (Separate Database)~~ ✅
26. ~~Polyglot Persistence~~ ✅

---

## Notes

- Each implementation should include:
  - Docker Compose setup
  - Go application demonstrating the pattern
  - DOCUMENTATION.md with line-by-line explanation
  - Pros/cons analysis
  - When to use guidance
- Make sure you're not using outdated methods either in golang or in docker and sql
