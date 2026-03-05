# Database Partitioning Documentation

This document provides a detailed explanation of the PostgreSQL database partitioning simulation project.

---

## Table of Contents

1. [Overview](#overview)
2. [What is Database Partitioning?](#what-is-database-partitioning)
3. [Project Architecture](#project-architecture)
4. [Docker Compose Setup](#docker-compose-setup)
5. [SQL Schema Explanation](#sql-schema-explanation)
6. [Go Application Walkthrough](#go-application-walkthrough)
7. [Running the Project](#running-the-project)

---

## Overview

This project demonstrates three PostgreSQL partitioning strategies:

| Strategy  | Table      | Partition Key | Use Case                                   |
| --------- | ---------- | ------------- | ------------------------------------------ |
| **Range** | `orders`   | `order_date`  | Time-series data, logs, historical records |
| **Hash**  | `users`    | `id`          | Even distribution across partitions        |
| **List**  | `products` | `category`    | Categorical data with known values         |

---

## What is Database Partitioning?

**Partitioning** splits a large table into smaller, more manageable pieces called **partitions**. Each partition stores a subset of the data based on a **partition key**.

### Benefits

- **Improved Query Performance**: Queries only scan relevant partitions (partition pruning)
- **Easier Maintenance**: Archive or drop old partitions without affecting other data
- **Parallel Processing**: Operations can run on multiple partitions simultaneously
- **Better Index Performance**: Smaller indexes per partition

### Partitioning Types in PostgreSQL

| Type      | Description                          | Example                 |
| --------- | ------------------------------------ | ----------------------- |
| **Range** | Data divided by value ranges         | Orders by date quarters |
| **Hash**  | Data distributed using hash function | Users by ID modulus     |
| **List**  | Data grouped by explicit value lists | Products by category    |

---

## Project Architecture

```
simulate_database_partitioning/
├── docker compose.yml          # PostgreSQL container configuration
├── init/
│   └── 01_create_partitioned_table.sql  # Database schema with partitions
├── main.go                     # Go application demonstrating partitioning
├── go.mod                      # Go module dependencies
├── README.md                   # Quick start guide
└── DOCUMENTATION.md            # This file
```

---

## Docker Compose Setup

### File: `docker compose.yml`

```yaml
services:
  postgres:
    image: postgres:18-bookworm
    container_name: partitioned_postgres
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: partitioned_db
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql
      - ./init:/docker-entrypoint-initdb.d
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 5s
      retries: 5

volumes:
  postgres_data:
```

### Line-by-Line Explanation

| Line  | Code                                   | Explanation                                                   |
| ----- | -------------------------------------- | ------------------------------------------------------------- |
| 1     | `services:`                            | Defines Docker services (no version needed in modern Compose) |
| 3     | `image: postgres:18-bookworm`          | Uses PostgreSQL 18 on Debian Bookworm                         |
| 4     | `container_name: partitioned_postgres` | Names the container for easy reference                        |
| 5-8   | `environment:`                         | Sets PostgreSQL credentials and database name                 |
| 9-10  | `ports: "5432:5432"`                   | Maps container port 5432 to host port 5432                    |
| 11-13 | `volumes:`                             | Persists data and mounts init scripts                         |
| 12    | `postgres_data:/var/lib/postgresql`    | Named volume at parent dir (PostgreSQL 18+ requirement)       |
| 13    | `./init:/docker-entrypoint-initdb.d`   | Auto-executes SQL files on first startup                      |
| 14-18 | `healthcheck:`                         | Verifies PostgreSQL is ready to accept connections            |

> **Note:** PostgreSQL 18+ stores data in version-specific subdirectories (e.g., `/var/lib/postgresql/data/18/`). The mount point must be `/var/lib/postgresql` (not `/var/lib/postgresql/data`) to support `pg_upgrade`.

---

## SQL Schema Explanation

### File: `init/01_create_partitioned_table.sql`

This file creates three partitioned tables demonstrating different strategies.

---

### 1. Range Partitioning (Orders Table)

```sql
CREATE TABLE orders (
    id SERIAL,
    order_date DATE NOT NULL,
    customer_id INTEGER NOT NULL,
    amount DECIMAL(10, 2) NOT NULL,
    status VARCHAR(50) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id, order_date)
) PARTITION BY RANGE (order_date);
```

#### Line-by-Line Explanation

| Line | Code                              | Explanation                                                   |
| ---- | --------------------------------- | ------------------------------------------------------------- |
| 1    | `CREATE TABLE orders`             | Creates the parent (partitioned) table                        |
| 2    | `id SERIAL`                       | Auto-incrementing integer ID                                  |
| 3    | `order_date DATE NOT NULL`        | **Partition key** - determines which partition stores the row |
| 4-6  | `customer_id, amount, status`     | Regular columns for order data                                |
| 7    | `created_at TIMESTAMP`            | Automatic timestamp on insert                                 |
| 8    | `PRIMARY KEY (id, order_date)`    | **Important**: Partition key must be part of primary key      |
| 9    | `PARTITION BY RANGE (order_date)` | Declares range partitioning on `order_date`                   |

#### Creating Range Partitions

```sql
CREATE TABLE orders_2024_q1 PARTITION OF orders
    FOR VALUES FROM ('2024-01-01') TO ('2024-04-01');
```

| Part                     | Explanation                                             |
| ------------------------ | ------------------------------------------------------- |
| `orders_2024_q1`         | Partition table name (stores Q1 2024 orders)            |
| `PARTITION OF orders`    | Links this partition to the parent table                |
| `FOR VALUES FROM ... TO` | Defines the date range (inclusive start, exclusive end) |

**Range boundaries:**

- `FROM ('2024-01-01')` - Includes January 1, 2024
- `TO ('2024-04-01')` - Excludes April 1, 2024 (goes to next partition)

---

### 2. Hash Partitioning (Users Table)

```sql
CREATE TABLE users (
    id INTEGER NOT NULL,
    username VARCHAR(100) NOT NULL,
    email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) PARTITION BY HASH (id);
```

#### Line-by-Line Explanation

| Line | Code                     | Explanation                                    |
| ---- | ------------------------ | ---------------------------------------------- |
| 1    | `CREATE TABLE users`     | Creates the parent table                       |
| 2    | `id INTEGER NOT NULL`    | **Partition key** - hash is computed from this |
| 3-4  | `username, email`        | User profile data                              |
| 5    | `PRIMARY KEY (id)`       | Primary key (partition key included)           |
| 6    | `PARTITION BY HASH (id)` | Declares hash partitioning on `id`             |

#### Creating Hash Partitions

```sql
CREATE TABLE users_p0 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 0);
CREATE TABLE users_p1 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 1);
CREATE TABLE users_p2 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 2);
CREATE TABLE users_p3 PARTITION OF users FOR VALUES WITH (MODULUS 4, REMAINDER 3);
```

| Parameter       | Explanation                                                |
| --------------- | ---------------------------------------------------------- |
| `MODULUS 4`     | Total number of partitions (divides hash space into 4)     |
| `REMAINDER 0-3` | Which partition gets rows where `hash(id) % 4 = remainder` |

**How it works:**

1. PostgreSQL computes `hash(user_id)`
2. Calculates `hash(user_id) % 4`
3. Routes row to partition matching the remainder

---

### 3. List Partitioning (Products Table)

```sql
CREATE TABLE products (
    id SERIAL,
    name VARCHAR(255) NOT NULL,
    category VARCHAR(50) NOT NULL,
    price DECIMAL(10, 2) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id, category)
) PARTITION BY LIST (category);
```

#### Line-by-Line Explanation

| Line | Code                            | Explanation                                             |
| ---- | ------------------------------- | ------------------------------------------------------- |
| 1    | `CREATE TABLE products`         | Creates the parent table                                |
| 2    | `id SERIAL`                     | Auto-incrementing product ID                            |
| 3    | `category VARCHAR(50) NOT NULL` | **Partition key** - determines partition by exact value |
| 4-5  | `name, price`                   | Product details                                         |
| 6    | `PRIMARY KEY (id, category)`    | Partition key must be in primary key                    |
| 7    | `PARTITION BY LIST (category)`  | Declares list partitioning on `category`                |

#### Creating List Partitions

```sql
CREATE TABLE products_electronics PARTITION OF products
    FOR VALUES IN ('electronics', 'computers', 'phones');

CREATE TABLE products_clothing PARTITION OF products
    FOR VALUES IN ('clothing', 'shoes', 'accessories');

CREATE TABLE products_home PARTITION OF products
    FOR VALUES IN ('furniture', 'kitchen', 'garden');

CREATE TABLE products_other PARTITION OF products DEFAULT;
```

| Partition              | Categories                     | Explanation                   |
| ---------------------- | ------------------------------ | ----------------------------- |
| `products_electronics` | electronics, computers, phones | Tech products                 |
| `products_clothing`    | clothing, shoes, accessories   | Fashion items                 |
| `products_home`        | furniture, kitchen, garden     | Home goods                    |
| `products_other`       | DEFAULT                        | Catches any unlisted category |

**Important:** The `DEFAULT` partition catches rows that don't match any defined list.

---

## Go Application Walkthrough

### File: `main.go`

---

### Imports and Constants

```go
import (
    "context"
    "fmt"
    "log"
    "math/rand"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

const (
    dbURL = "postgres://postgres:postgres@localhost:5432/partitioned_db"
)
```

| Import/Constant     | Purpose                                           |
| ------------------- | ------------------------------------------------- |
| `context`           | Manages request lifecycle and cancellation        |
| `fmt`, `log`        | Output and error logging                          |
| `math/rand`, `time` | Random data generation                            |
| `pgx/v5`            | PostgreSQL driver for Go (high-performance)       |
| `pgxpool`           | Connection pooling for concurrent database access |
| `dbURL`             | PostgreSQL connection string                      |

---

### Main Function

```go
func main() {
    ctx := context.Background()

    pool, err := pgxpool.New(ctx, dbURL)
    if err != nil {
        log.Fatalf("Unable to connect to database: %v", err)
    }
    defer pool.Close()
    // ... demo functions
}
```

| Line | Code                          | Explanation                                  |
| ---- | ----------------------------- | -------------------------------------------- |
| 1    | `ctx := context.Background()` | Creates root context for database operations |
| 2    | `pgxpool.New(ctx, dbURL)`     | Creates connection pool to PostgreSQL        |
| 3-4  | Error handling                | Exits if connection fails                    |
| 5    | `defer pool.Close()`          | Ensures connections are released on exit     |

---

### demonstrateRangePartitioning Function

```go
func demonstrateRangePartitioning(ctx context.Context, pool *pgxpool.Pool) {
    orders := []struct {
        orderDate  string
        customerID int
        amount     float64
        status     string
    }{
        {"2024-01-15", 1, 150.00, "completed"},
        // ... more orders
    }

    for _, o := range orders {
        _, err := pool.Exec(ctx,
            "INSERT INTO orders (order_date, customer_id, amount, status) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
            o.orderDate, o.customerID, o.amount, o.status)
        // ...
    }
}
```

| Part                                     | Explanation                                             |
| ---------------------------------------- | ------------------------------------------------------- |
| `orders := []struct{...}`                | Anonymous struct slice holding sample order data        |
| `{"2024-01-15", 1, 150.00, "completed"}` | Order from Q1 2024 → goes to `orders_2024_q1` partition |
| `pool.Exec(ctx, ...)`                    | Executes INSERT statement                               |
| `$1, $2, $3, $4`                         | Parameterized query (prevents SQL injection)            |
| `ON CONFLICT DO NOTHING`                 | Ignores duplicate inserts (idempotent)                  |

**Partition routing:** PostgreSQL automatically routes each order to the correct partition based on `order_date`.

---

### demonstrateHashPartitioning Function

```go
func demonstrateHashPartitioning(ctx context.Context, pool *pgxpool.Pool) {
    users := []struct {
        id       int
        username string
        email    string
    }{
        {1, "alice", "alice@example.com"},
        // ... more users
    }

    // Insert users
    for _, u := range users {
        _, err := pool.Exec(ctx,
            "INSERT INTO users (id, username, email) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
            u.id, u.username, u.email)
    }

    // Show distribution
    partitions := []string{"users_p0", "users_p1", "users_p2", "users_p3"}
    for _, p := range partitions {
        var count int
        err := pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", p)).Scan(&count)
        fmt.Printf("   %s: %d users\n", p, count)
    }
}
```

| Part                              | Explanation                                          |
| --------------------------------- | ---------------------------------------------------- |
| `users := []struct{...}`          | Sample user data with explicit IDs                   |
| `pool.QueryRow(...).Scan(&count)` | Queries each partition directly to show distribution |
| `SELECT COUNT(*) FROM users_p0`   | Counts rows in specific partition                    |

**Hash distribution:** Users are distributed based on `hash(id) % 4`. Distribution may not be perfectly even with small datasets.

---

### demonstrateListPartitioning Function

```go
func demonstrateListPartitioning(ctx context.Context, pool *pgxpool.Pool) {
    products := []struct {
        name     string
        category string
        price    float64
    }{
        {"iPhone 15", "phones", 999.00},      // → products_electronics
        {"Nike Shoes", "shoes", 150.00},       // → products_clothing
        {"Dining Table", "furniture", 599.00}, // → products_home
        {"Book: Go Programming", "books", 49.99}, // → products_other (DEFAULT)
    }
    // ...
}
```

| Product              | Category  | Partition                  |
| -------------------- | --------- | -------------------------- |
| iPhone 15            | phones    | `products_electronics`     |
| Nike Shoes           | shoes     | `products_clothing`        |
| Dining Table         | furniture | `products_home`            |
| Book: Go Programming | books     | `products_other` (DEFAULT) |

---

### showPartitionStats Function

```go
func showPartitionStats(ctx context.Context, pool *pgxpool.Pool) {
    query := `
        SELECT
            parent.relname AS parent_table,
            child.relname AS partition_name,
            pg_size_pretty(pg_relation_size(child.oid)) AS partition_size
        FROM pg_inherits
        JOIN pg_class parent ON pg_inherits.inhparent = parent.oid
        JOIN pg_class child ON pg_inherits.inhrelid = child.oid
        WHERE parent.relname IN ('orders', 'users', 'products')
        ORDER BY parent.relname, child.relname
    `
    // ...
}
```

| SQL Part                                | Explanation                                                |
| --------------------------------------- | ---------------------------------------------------------- |
| `pg_inherits`                           | System catalog storing partition inheritance relationships |
| `pg_class`                              | System catalog with table metadata                         |
| `parent.relname`                        | Parent (partitioned) table name                            |
| `child.relname`                         | Child (partition) table name                               |
| `pg_size_pretty(pg_relation_size(...))` | Human-readable partition size (e.g., "8192 bytes")         |

---

### demonstratePartitionPruning Function

```go
func demonstratePartitionPruning(ctx context.Context, pool *pgxpool.Pool) {
    // Batch insert 1000 random orders
    batch := &pgx.Batch{}
    for i := 0; i < 1000; i++ {
        days := rand.Intn(820)
        orderDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
        // ...
        batch.Queue("INSERT INTO orders ...", orderDate, customerID, amount, status)
    }
    br := pool.SendBatch(ctx, batch)
    br.Exec()
    br.Close()

    // Query with partition pruning
    rows, _ := pool.Query(ctx,
        "EXPLAIN SELECT * FROM orders WHERE order_date >= '2024-01-01' AND order_date < '2024-04-01'")
}
```

| Part                         | Explanation                                      |
| ---------------------------- | ------------------------------------------------ |
| `pgx.Batch{}`                | Batches multiple queries for efficient execution |
| `batch.Queue(...)`           | Adds query to batch                              |
| `pool.SendBatch(ctx, batch)` | Sends all queries in single round-trip           |
| `EXPLAIN SELECT ...`         | Shows query execution plan                       |

**Partition Pruning Output Example:**

```
Seq Scan on orders_2024_q1 orders  (cost=0.00..15.00 rows=100 width=52)
  Filter: ((order_date >= '2024-01-01') AND (order_date < '2024-04-01'))
```

Notice: Only `orders_2024_q1` is scanned, not all partitions!

---

## Running the Project

### Step 1: Start PostgreSQL

```bash
docker compose up -d
```

This:

1. Pulls `postgres:18-bookworm` image
2. Creates container `partitioned_postgres`
3. Executes `init/01_create_partitioned_table.sql` automatically
4. Creates all partitioned tables

### Step 2: Verify Database is Ready

```bash
docker compose logs -f postgres
```

Wait for: `database system is ready to accept connections`

### Step 3: Install Go Dependencies

```bash
go mod tidy
```

Downloads `pgx/v5` driver and dependencies.

### Step 4: Run the Demo

```bash
go run main.go
```

### Expected Output

```
Connected to PostgreSQL!
=

📊 RANGE PARTITIONING DEMO (Orders by Date)
-
✅ Inserted orders across multiple quarters (2024-2026)

📋 Orders from Q1 2024 (partition: orders_2024_q1):
   ID: 1, Date: 2024-01-15, Customer: 1, Amount: $150.00, Status: completed
   ID: 2, Date: 2024-02-20, Customer: 2, Amount: $250.50, Status: completed

🔢 HASH PARTITIONING DEMO (Users by ID)
-
✅ Inserted users (distributed across 4 hash partitions)

📋 User distribution across hash partitions:
   users_p0: 1 users
   users_p1: 3 users
   users_p2: 1 users
   users_p3: 3 users

📝 LIST PARTITIONING DEMO (Products by Category)
-
✅ Inserted products across category partitions

📋 Product distribution across list partitions:
   Electronics (electronics, computers, phones): 3 products
   Clothing (clothing, shoes, accessories): 2 products
   Home (furniture, kitchen, garden): 3 products
   Other (default partition): 1 products

📈 PARTITION STATISTICS
-
📊 Partition Information:

   Table: orders
      └── orders_2024_q1 (size: 8192 bytes)
      └── orders_2024_q2 (size: 8192 bytes)
      └── orders_2024_q3 (size: 8192 bytes)
      └── orders_2024_q4 (size: 8192 bytes)
      └── orders_2025_q1 (size: 8192 bytes)
      └── orders_2025_q2 (size: 8192 bytes)
      └── orders_2025_q3 (size: 8192 bytes)
      └── orders_2025_q4 (size: 0 bytes)
      └── orders_2026_q1 (size: 8192 bytes)
      └── orders_2026_q2 (size: 0 bytes)

   Table: products
      └── products_clothing (size: 8192 bytes)
      └── products_electronics (size: 8192 bytes)
      └── products_home (size: 8192 bytes)
      └── products_other (size: 8192 bytes)

   Table: users
      └── users_p0 (size: 8192 bytes)
      └── users_p1 (size: 8192 bytes)
      └── users_p2 (size: 8192 bytes)
      └── users_p3 (size: 8192 bytes)

⚡ QUERY PERFORMANCE WITH PARTITION PRUNING
-
📥 Inserting 1000 random orders for performance testing...
✅ Inserted 1000 orders

🔍 Query with partition pruning (Q1 2024 only):
   Found 109 orders in Q1 2024 (took 351.864µs)

📋 EXPLAIN output showing partition pruning:
   Seq Scan on orders_2024_q1 orders  (cost=0.00..16.60 rows=2 width=154)
     Filter: ((order_date >= '2024-01-01'::date) AND (order_date < '2024-04-01'::date))
```

### Step 5: Cleanup

```bash
docker compose down -v
```

Removes container and deletes data volume.

---

## Key Takeaways

1. **Partition key must be in PRIMARY KEY** - PostgreSQL requires this for uniqueness enforcement
2. **Partition pruning is automatic** - PostgreSQL optimizer skips irrelevant partitions
3. **DEFAULT partitions catch unmatched values** - Prevents insert failures for list partitioning
4. **Hash partitioning distributes evenly** - Good for load balancing across partitions
5. **Range partitioning is ideal for time-series** - Easy to archive old partitions
