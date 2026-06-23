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

---

## To Implement

### Replication Patterns

- [ ] **Multi-Primary Replication**
  - Multiple writable nodes
  - Conflict resolution strategies
  - Use cases: geographic distribution, high availability

- [ ] **Synchronous vs Asynchronous Replication**
  - Trade-offs between consistency and performance
  - Quorum-based replication

### Caching Patterns

- [ ] **Materialized Views**
  - Pre-computed query results
  - Refresh strategies (on-demand, periodic, incremental)

### Connection Management

- [ ] **Connection Load Balancing**
  - HAProxy / pgpool for distributing connections
  - Health checks and failover

### Data Distribution Patterns

- [x] **Consistent Hashing** - `simulate_consistent_hashing/`
  - Virtual nodes for better distribution
  - Minimal data movement on node changes
  - Comparison with modulo-based sharding

- [ ] **Directory-Based Sharding**
  - Lookup service for shard mapping
  - Dynamic shard assignment
  - Trade-offs vs hash-based routing

- [ ] **Range-Based Sharding**
  - Sharding by date ranges, geographic regions
  - Handling hotspots and rebalancing

### Vertical Scaling Techniques

- [ ] **Vertical Partitioning**
  - Splitting tables by columns
  - Separating hot and cold data
  - BLOB/CLOB storage separation

- [ ] **Database Denormalization**
  - Strategic redundancy for read performance
  - Maintaining consistency with triggers/application logic

### Distributed Database Patterns

- [ ] **Federation / Functional Partitioning**
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

- [ ] **Hot Standby**
  - Warm standby vs hot standby
  - Streaming replication setup
  - Point-in-time recovery (PITR)

### Query Optimization Patterns

- [ ] **Database Indexing Strategies**
  - B-tree, Hash, GIN, GiST indexes
  - Partial indexes, covering indexes
  - Index maintenance and bloat

### Data Archival & Tiering

- [ ] **Data Archival**
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

- [ ] **Shared Database, Separate Schema**
  - Schema per tenant
  - Connection routing

- [ ] **Separate Database per Tenant**
  - Full isolation
  - Resource management challenges

### Emerging Patterns

- [x] **NewSQL Databases** - `simulate_newsql/`
  - CockroachDB, TiDB, YugabyteDB
  - Distributed SQL with ACID guarantees
  - Comparison with traditional sharding

- [ ] **Polyglot Persistence**
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

---

## Notes

- Each implementation should include:
  - Docker Compose setup
  - Go application demonstrating the pattern
  - DOCUMENTATION.md with line-by-line explanation
  - Pros/cons analysis
  - When to use guidance
- Make sure you're not using outdated methods either in golang or in docker and sql
