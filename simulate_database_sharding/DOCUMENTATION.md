# Database Sharding Simulation - Line-by-Line Documentation

This document provides a comprehensive line-by-line explanation of the `simulate_database_sharding` project, covering **what** each section does, **why** it's implemented that way, and the **pros and cons** of each design decision.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure (docker-compose.yml)](#infrastructure-docker-composeyml)
3. [Database Schema (01_create_tables.sql)](#database-schema-01_create_tablessql)
4. [Application Code (main.go)](#application-code-maingo)
5. [Summary: Pros and Cons of Database Sharding](#summary-pros-and-cons-of-database-sharding)

---

## Project Overview

**Database sharding** is a horizontal scaling technique that distributes data across multiple independent database instances (shards). Each shard holds a subset of the total data, determined by a **shard key** (in this case, `user_id`).

### Why Sharding?

- **Scalability**: Single databases have limits (CPU, memory, disk I/O). Sharding distributes load across multiple servers.
- **Performance**: Queries only hit the relevant shard, reducing data scanned.
- **Availability**: Failure of one shard doesn't affect others (partial availability).

---

## Infrastructure (docker-compose.yml)

### Lines 1-19: Shard 1 Configuration

```yaml
services:
  # Shard 1 - Users with ID hash % 3 == 0
  postgres_shard1:
    image: postgres:18-bookworm
    container_name: shard1_postgres
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: shard_db
    ports:
      - "5433:5432"
    volumes:
      - shard1_data:/var/lib/postgresql
      - ./init/shard1:/docker-entrypoint-initdb.d
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 5s
      retries: 5
```

| Line                              | What It Does                               | Why We Do It                                           |
| --------------------------------- | ------------------------------------------ | ------------------------------------------------------ |
| `image: postgres:18-bookworm`     | Uses PostgreSQL 18 on Debian Bookworm      | Stable, production-ready base image                    |
| `container_name: shard1_postgres` | Names the container explicitly             | Easier debugging and identification                    |
| `POSTGRES_USER/PASSWORD/DB`       | Sets up database credentials               | Required for PostgreSQL initialization                 |
| `ports: "5433:5432"`              | Maps container port 5432 to host port 5433 | Each shard needs a unique host port (5433, 5434, 5435) |
| `volumes: shard1_data`            | Persists database data                     | Data survives container restarts                       |
| `volumes: ./init/shard1`          | Mounts init scripts                        | Auto-runs SQL on first startup                         |
| `healthcheck`                     | Monitors container health                  | Ensures database is ready before app connects          |

**Pros:**

- Each shard is completely isolated (separate container, data volume)
- Easy to scale by adding more shard definitions
- Health checks prevent premature connection attempts

**Cons:**

- Manual port management required
- No automatic shard discovery (hardcoded in app)
- Each shard needs identical schema maintenance

### Lines 21-57: Shards 2 and 3

Identical configuration to Shard 1, differing only in:

- Container name (`shard2_postgres`, `shard3_postgres`)
- Host port (`5434`, `5435`)
- Init script path (`./init/shard2`, `./init/shard3`)

### Lines 59-63: Volume Definitions

```yaml
volumes:
  shard1_data:
  shard2_data:
  shard3_data:
```

| What It Does           | Why We Do It                                                                |
| ---------------------- | --------------------------------------------------------------------------- |
| Declares named volumes | Docker manages storage location; data persists across container recreations |

---

## Database Schema (01_create_tables.sql)

Each shard has identical schema. This is **critical** for sharding to work correctly.

### Lines 1-10: Users Table

```sql
-- Shard 1: Tables for this shard
-- This shard handles users where hash(user_id) % 3 == 0

CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    username VARCHAR(100) NOT NULL,
    email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

| Line                           | What It Does                 | Why We Do It                                                                           |
| ------------------------------ | ---------------------------- | -------------------------------------------------------------------------------------- |
| `id INTEGER PRIMARY KEY`       | User ID is the **shard key** | Application controls ID assignment; no auto-increment to avoid conflicts across shards |
| `username VARCHAR(100)`        | Stores username              | Non-unique across shards (same username could theoretically exist on different shards) |
| `created_at TIMESTAMP DEFAULT` | Auto-timestamps creation     | Audit trail without application logic                                                  |

**Why `INTEGER` instead of `SERIAL`?**

- The application assigns IDs, not the database
- `SERIAL` would create conflicting IDs across shards (each shard would have user 1, 2, 3...)
- The shard key must be known **before** insertion to route to the correct shard

### Lines 12-20: Orders Table

```sql
CREATE TABLE orders (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL,
    order_date DATE NOT NULL,
    amount DECIMAL(10, 2) NOT NULL,
    status VARCHAR(50) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

| Line                       | What It Does               | Why We Do It                                                           |
| -------------------------- | -------------------------- | ---------------------------------------------------------------------- |
| `id SERIAL PRIMARY KEY`    | Auto-incrementing order ID | Order IDs can overlap across shards (order #1 exists on each shard)    |
| `user_id INTEGER NOT NULL` | Links to user              | **Co-location key**: orders are stored on the same shard as their user |
| `amount DECIMAL(10, 2)`    | Precise currency storage   | Avoids floating-point rounding errors                                  |

**Co-location Strategy:**

- Orders are routed using `user_id`, not `order_id`
- This ensures all orders for a user are on the same shard
- Enables efficient joins between `users` and `orders` within a single shard

**Pros:**

- Single-shard queries for user + orders (no cross-shard joins)
- Maintains referential integrity within shard

**Cons:**

- Cannot have foreign key constraints (user might not exist on this shard during migration)
- Order IDs are not globally unique

### Lines 22-26: Indexes

```sql
CREATE INDEX idx_orders_user_id ON orders (user_id);
CREATE INDEX idx_orders_status ON orders (status);
CREATE INDEX idx_users_username ON users (username);
```

| Index                | Purpose                                                   |
| -------------------- | --------------------------------------------------------- |
| `idx_orders_user_id` | Fast lookup of orders by user (most common query pattern) |
| `idx_orders_status`  | Filter orders by status (pending, shipped, etc.)          |
| `idx_users_username` | Support scatter-gather queries searching by username      |

---

## Application Code (main.go)

### Lines 1-13: Package and Imports

```go
package main

import (
    "context"
    "fmt"
    "hash/fnv"
    "log"
    "math/rand"
    "sync"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
)
```

| Import     | Purpose                                                 |
| ---------- | ------------------------------------------------------- |
| `context`  | Manages request lifecycles and cancellation             |
| `hash/fnv` | FNV-1a hash algorithm for consistent shard routing      |
| `sync`     | WaitGroup for parallel shard queries                    |
| `pgxpool`  | PostgreSQL connection pooling (high-performance driver) |

**Why `pgxpool` instead of `database/sql`?**

- Native PostgreSQL protocol support
- Better performance for high-concurrency workloads
- Built-in connection pooling

### Lines 15-25: Data Structures

```go
type ShardConfig struct {
    Name string
    URL  string
}

type ShardManager struct {
    shards []*pgxpool.Pool
    count  int
}
```

| Field                 | Purpose                                         |
| --------------------- | ----------------------------------------------- |
| `ShardConfig.Name`    | Human-readable shard identifier for logging     |
| `ShardConfig.URL`     | PostgreSQL connection string                    |
| `ShardManager.shards` | Array of connection pools (one per shard)       |
| `ShardManager.count`  | Number of shards (cached for modulo operations) |

**Design Decision:** The `ShardManager` is the **routing layer** between the application and databases. All shard selection logic is centralized here.

### Lines 27-48: NewShardManager Constructor

```go
func NewShardManager(ctx context.Context, configs []ShardConfig) (*ShardManager, error) {
    sm := &ShardManager{
        shards: make([]*pgxpool.Pool, len(configs)),
        count:  len(configs),
    }

    for i, cfg := range configs {
        pool, err := pgxpool.New(ctx, cfg.URL)
        if err != nil {
            // Close any already opened connections
            for j := 0; j < i; j++ {
                sm.shards[j].Close()
            }
            return nil, fmt.Errorf("failed to connect to shard %s: %w", cfg.Name, err)
        }
        sm.shards[i] = pool
        fmt.Printf("✅ Connected to %s\n", cfg.Name)
    }

    return sm, nil
}
```

| Lines | What It Does                       | Why We Do It                                               |
| ----- | ---------------------------------- | ---------------------------------------------------------- |
| 29-32 | Pre-allocate shard array           | Avoids dynamic resizing                                    |
| 34-44 | Connect to each shard sequentially | Establishes connection pools                               |
| 37-40 | Cleanup on failure                 | **Critical**: Prevents connection leaks if one shard fails |
| 44    | Log successful connection          | Operational visibility                                     |

**Error Handling Pattern:**

- If shard 2 fails to connect, shards 0 and 1 are properly closed
- Returns wrapped error with shard name for debugging

**Pros:**

- Clean resource management
- Fail-fast behavior (app won't start with missing shards)

**Cons:**

- All shards must be available at startup
- No retry logic or circuit breaker

### Lines 50-55: Close Method

```go
func (sm *ShardManager) Close() {
    for _, pool := range sm.shards {
        pool.Close()
    }
}
```

Ensures all database connections are properly released. Called via `defer sm.Close()` in main.

### Lines 57-62: GetShardIndex - The Hashing Function

```go
func (sm *ShardManager) GetShardIndex(userID int) int {
    h := fnv.New32a()
    h.Write([]byte(fmt.Sprintf("%d", userID)))
    return int(h.Sum32()) % sm.count
}
```

**This is the core sharding logic.**

| Line                        | What It Does                 | Why We Do It                                       |
| --------------------------- | ---------------------------- | -------------------------------------------------- |
| `fnv.New32a()`              | Creates FNV-1a 32-bit hash   | Fast, good distribution, deterministic             |
| `fmt.Sprintf("%d", userID)` | Converts int to string bytes | Hash functions operate on byte slices              |
| `h.Sum32() % sm.count`      | Maps hash to shard index     | Modulo ensures result is 0, 1, or 2 (for 3 shards) |

**Why FNV-1a?**

- **Fast**: Single-pass, no complex math
- **Deterministic**: Same input always produces same output
- **Good distribution**: Minimizes hotspots

**Why not simple `userID % 3`?**

- Sequential IDs would create uneven distribution (users 1,4,7,10 all on shard 1)
- Hashing spreads sequential IDs across shards more evenly

**Pros:**

- O(1) shard lookup
- No external dependencies (no lookup table)
- Consistent across application restarts

**Cons:**

- **Resharding is expensive**: Adding a 4th shard changes `% 3` to `% 4`, requiring data migration
- Not truly "consistent hashing" (which handles node changes better)

### Lines 64-77: Shard Access Methods

```go
func (sm *ShardManager) GetShard(userID int) *pgxpool.Pool {
    return sm.shards[sm.GetShardIndex(userID)]
}

func (sm *ShardManager) GetShardByIndex(index int) *pgxpool.Pool {
    return sm.shards[index]
}

func (sm *ShardManager) GetAllShards() []*pgxpool.Pool {
    return sm.shards
}
```

| Method                   | Use Case                               |
| ------------------------ | -------------------------------------- |
| `GetShard(userID)`       | Route by shard key (most common)       |
| `GetShardByIndex(index)` | Direct shard access (admin operations) |
| `GetAllShards()`         | Scatter-gather queries                 |

### Lines 79-95: Domain Models

```go
type User struct {
    ID        int
    Username  string
    Email     string
    CreatedAt time.Time
}

type Order struct {
    ID        int
    UserID    int
    OrderDate time.Time
    Amount    float64
    Status    string
    CreatedAt time.Time
}
```

Standard Go structs mapping to database tables. Note `UserID` in `Order` - this is the **co-location key**.

### Lines 97-142: Main Function

```go
func main() {
    ctx := context.Background()

    shardConfigs := []ShardConfig{
        {Name: "Shard 1", URL: "postgres://postgres:postgres@localhost:5433/shard_db?sslmode=disable"},
        {Name: "Shard 2", URL: "postgres://postgres:postgres@localhost:5434/shard_db?sslmode=disable"},
        {Name: "Shard 3", URL: "postgres://postgres:postgres@localhost:5435/shard_db?sslmode=disable"},
    }

    sm, err := NewShardManager(ctx, shardConfigs)
    if err != nil {
        log.Fatalf("Failed to initialize shard manager: %v", err)
    }
    defer sm.Close()
    // ... demo functions
}
```

| Lines   | What It Does                   | Why We Do It                                           |
| ------- | ------------------------------ | ------------------------------------------------------ |
| 98      | Background context             | No timeout for demo; production would use timeouts     |
| 101-105 | Hardcoded shard configs        | Demo simplicity; production uses config files/env vars |
| 108-111 | Initialize with fatal on error | App cannot function without all shards                 |
| 112     | Defer close                    | Ensures cleanup even on panic                          |

**Cons of hardcoded config:**

- No dynamic shard discovery
- Credentials in source code (security risk)
- Requires code change to add shards

### Lines 144-184: demonstrateUserSharding

```go
func demonstrateUserSharding(ctx context.Context, sm *ShardManager) {
    users := []User{
        {ID: 1, Username: "alice", Email: "alice@example.com"},
        // ... more users
    }

    for _, u := range users {
        shardIdx := sm.GetShardIndex(u.ID)
        shard := sm.GetShard(u.ID)

        _, err := shard.Exec(ctx,
            "INSERT INTO users (id, username, email) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING",
            u.ID, u.Username, u.Email)
        // ...
    }
}
```

**Key Pattern: Route-then-Execute**

1. **Determine shard** using `GetShardIndex(u.ID)`
2. **Get connection** using `GetShard(u.ID)`
3. **Execute query** on that specific shard

| SQL Element                   | Purpose                                           |
| ----------------------------- | ------------------------------------------------- |
| `ON CONFLICT (id) DO NOTHING` | Idempotent inserts (re-running demo doesn't fail) |
| `$1, $2, $3`                  | Parameterized queries (SQL injection prevention)  |

**Lines 173-183: Reading from correct shard**

```go
userID := 5
shard := sm.GetShard(userID)
err := shard.QueryRow(ctx, "SELECT username, email FROM users WHERE id = $1", userID).Scan(&username, &email)
```

This demonstrates **direct shard access** - when you know the shard key, you query exactly one database.

### Lines 186-235: demonstrateOrderSharding

```go
for userID := 1; userID <= 10; userID++ {
    shard := sm.GetShard(userID)  // Route by USER ID, not order ID
    // ... insert orders
}
```

**Critical Insight:** Orders are routed by `userID`, not `orderID`. This is **data co-location**.

**Why co-locate orders with users?**

- "Get all orders for user X" is a single-shard query
- Joins between `users` and `orders` work within one shard
- No distributed transactions needed for user+order operations

**Cons:**

- Cannot efficiently query "all orders over $100" without scatter-gather
- User with many orders creates a "hot shard"

### Lines 237-283: demonstrateScatterGather

```go
func demonstrateScatterGather(ctx context.Context, sm *ShardManager) {
    var wg sync.WaitGroup
    results := make(chan struct {
        shardIdx int
        user     *User
    }, sm.count)

    // Query all shards in parallel
    for i, shard := range sm.GetAllShards() {
        wg.Add(1)
        go func(idx int, pool *pgxpool.Pool) {
            defer wg.Done()
            // ... query this shard
            if err == nil {
                results <- struct{...}{idx, &user}
            }
        }(i, shard)
    }

    go func() {
        wg.Wait()
        close(results)
    }()

    for result := range results {
        // ... process results
    }
}
```

**Scatter-Gather Pattern:**

1. **Scatter**: Send query to ALL shards in parallel (goroutines)
2. **Gather**: Collect results via channel
3. **Merge**: Process combined results

| Component                | Purpose                            |
| ------------------------ | ---------------------------------- |
| `sync.WaitGroup`         | Track when all goroutines complete |
| `chan` with buffer       | Collect results without blocking   |
| Closure with `idx, pool` | Capture loop variables correctly   |

**When to use scatter-gather:**

- Searching by non-shard-key field (e.g., `username`)
- Global aggregations (total sales across all shards)
- Admin queries (count all users)

**Pros:**

- Parallel execution reduces latency
- Linear scalability with shard count

**Cons:**

- Queries ALL shards even if data is on one
- Network overhead multiplied by shard count
- Results must fit in memory for merging

### Lines 285-314: showShardStats

Demonstrates querying each shard for statistics and aggregating results. Shows data distribution across shards.

### Lines 316-393: demonstrateCrossShardAggregation

```go
func demonstrateCrossShardAggregation(ctx context.Context, sm *ShardManager) {
    type ShardStats struct {
        shardIdx   int
        totalSales float64
        orderCount int
        avgOrder   float64
    }

    // Parallel queries to all shards
    start := time.Now()
    for i, shard := range sm.GetAllShards() {
        wg.Add(1)
        go func(idx int, pool *pgxpool.Pool) {
            defer wg.Done()
            err := pool.QueryRow(ctx, `
                SELECT
                    COALESCE(SUM(amount), 0) as total_sales,
                    COUNT(*) as order_count,
                    COALESCE(AVG(amount), 0) as avg_order
                FROM orders
            `).Scan(&stats.totalSales, &stats.orderCount, &stats.avgOrder)
            // ...
        }(i, shard)
    }

    // Aggregate in application
    for stats := range results {
        globalTotalSales += stats.totalSales
        globalOrderCount += stats.orderCount
    }

    // Recalculate global average (can't just average the averages!)
    globalAvg = globalTotalSales / float64(globalOrderCount)
}
```

**Key Insight: Aggregation Logic**

| Aggregation | Can Merge Directly? | Correct Approach            |
| ----------- | ------------------- | --------------------------- |
| `SUM`       | ✅ Yes              | Sum of sums                 |
| `COUNT`     | ✅ Yes              | Sum of counts               |
| `AVG`       | ❌ No               | Sum of sums / Sum of counts |
| `MIN/MAX`   | ✅ Yes              | Min/Max of results          |
| `DISTINCT`  | ❌ No               | Merge and re-deduplicate    |

**Why `COALESCE`?**

- Empty shards return `NULL` for `SUM`/`AVG`
- `COALESCE(SUM(amount), 0)` converts `NULL` to `0`

---

## Summary: Pros and Cons of Database Sharding

### Pros

| Benefit                     | Explanation                                 |
| --------------------------- | ------------------------------------------- |
| **Horizontal Scalability**  | Add more shards to handle more data/traffic |
| **Performance**             | Queries only scan relevant shard's data     |
| **Isolation**               | Shard failures don't affect other shards    |
| **Geographic Distribution** | Shards can be in different regions          |
| **Resource Efficiency**     | Each shard can be sized independently       |

### Cons

| Drawback                 | Explanation                                    |
| ------------------------ | ---------------------------------------------- |
| **Complexity**           | Application must handle routing, aggregation   |
| **Cross-Shard Queries**  | Expensive scatter-gather operations            |
| **No Cross-Shard Joins** | Must denormalize or query multiple times       |
| **Transactions**         | Distributed transactions are complex/slow      |
| **Resharding**           | Adding/removing shards requires data migration |
| **Operational Overhead** | Multiple databases to monitor, backup, upgrade |
| **Data Skew**            | Poor shard key choice creates "hot" shards     |

### When to Use Sharding

**Good fit:**

- Data naturally partitions by a key (user_id, tenant_id)
- Single database can't handle write volume
- Data size exceeds single server capacity
- Need geographic data locality

**Avoid if:**

- Data requires frequent cross-entity joins
- Strong consistency across all data is required
- Operational complexity is a concern
- Data fits comfortably in one database

---

## Key Takeaways

1. **Shard key selection is critical** - Choose a key that distributes data evenly and matches query patterns
2. **Co-locate related data** - Store orders on the same shard as their user
3. **Plan for scatter-gather** - Some queries will need to hit all shards
4. **Aggregation happens in the application** - Database can't aggregate across shards
5. **Resharding is painful** - Plan shard count carefully upfront
6. **Test data distribution** - Monitor for hot shards and skewed data

## go run output

```shell
$  go run main.go
✅ Connected to Shard 1
✅ Connected to Shard 2
✅ Connected to Shard 3


🔀 DATABASE SHARDING SIMULATION


👥 USER SHARDING DEMO
-
📥 Inserting users across shards...
   User alice (ID: 1) → Shard 2
   User bob (ID: 2) → Shard 2
   User charlie (ID: 3) → Shard 3
   User diana (ID: 4) → Shard 2
   User eve (ID: 5) → Shard 3
   User frank (ID: 6) → Shard 3
   User grace (ID: 7) → Shard 1
   User henry (ID: 8) → Shard 2
   User ivy (ID: 9) → Shard 3
   User jack (ID: 10) → Shard 1

📖 Reading user by ID (direct shard access):
   Found user ID 5 on Shard 3: eve (eve@example.com)

📦 ORDER SHARDING DEMO (Co-located with Users)
-
📥 Inserting orders (co-located with their users)...
   User 1: 3 orders → Shard 2
   User 2: 4 orders → Shard 2
   User 3: 3 orders → Shard 3
   User 4: 3 orders → Shard 2
   User 5: 4 orders → Shard 3
   User 6: 3 orders → Shard 3
   User 7: 2 orders → Shard 1
   User 8: 2 orders → Shard 2
   User 9: 5 orders → Shard 3
   User 10: 3 orders → Shard 1

📖 Reading orders for user ID 3 (single shard query):
   Order #1: 2025-08-24, $342.25, pending
   Order #3: 2025-06-04, $494.69, shipped
   Order #2: 2025-05-31, $428.49, completed

🔍 SCATTER-GATHER QUERY DEMO
-
🔍 Searching for user 'alice' across all shards...
   ✅ Found on Shard 2: alice (ID: 1, Email: alice@example.com)

📊 SHARD STATISTICS
-
📊 Data distribution across shards:

   Shard 1:
      Users:  2
      Orders: 5

   Shard 2:
      Users:  4
      Orders: 12

   Shard 3:
      Users:  4
      Orders: 15

   📈 Total across all shards:
      Users:  10
      Orders: 32

📈 CROSS-SHARD AGGREGATION
-
📈 Aggregating order statistics across all shards...

   Shard 1:
      Total Sales: $1388.00
      Order Count: 5
      Avg Order:   $277.60

   Shard 2:
      Total Sales: $2880.14
      Order Count: 12
      Avg Order:   $240.01

   Shard 3:
      Total Sales: $3956.49
      Order Count: 15
      Avg Order:   $263.77

   🌍 Global Aggregation (took 513.089µs):
      Total Sales: $8224.63
      Total Orders: 32
      Global Avg Order: $257.02
```
