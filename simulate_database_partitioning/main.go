package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dbURL = "postgres://postgres:postgres@localhost:5432/partitioned_db?sslmode=disable"
)

func main() {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	fmt.Println("Connected to PostgreSQL!")
	fmt.Println(strings.Repeat("=", 50))

	// Demonstrate Range Partitioning (Orders)
	fmt.Println("\n📊 RANGE PARTITIONING DEMO (Orders by Date)")
	fmt.Println(strings.Repeat("-", 50))
	demonstrateRangePartitioning(ctx, pool)

	// Demonstrate Hash Partitioning (Users)
	fmt.Println("\n🔢 HASH PARTITIONING DEMO (Users by ID)")
	fmt.Println(strings.Repeat("-", 50))
	demonstrateHashPartitioning(ctx, pool)

	// Demonstrate List Partitioning (Products)
	fmt.Println("\n📝 LIST PARTITIONING DEMO (Products by Category)")
	fmt.Println(strings.Repeat("-", 50))
	demonstrateListPartitioning(ctx, pool)

	// Show partition information
	fmt.Println("\n📈 PARTITION STATISTICS")
	fmt.Println(strings.Repeat("-", 50))
	showPartitionStats(ctx, pool)

	// Demonstrate query performance with partition pruning
	fmt.Println("\n⚡ QUERY PERFORMANCE WITH PARTITION PRUNING")
	fmt.Println(strings.Repeat("-", 50))
	demonstratePartitionPruning(ctx, pool)
}

func demonstrateRangePartitioning(ctx context.Context, pool *pgxpool.Pool) {
	// Insert orders across different quarters
	orders := []struct {
		orderDate  string
		customerID int
		amount     float64
		status     string
	}{
		{"2024-01-15", 1, 150.00, "completed"},
		{"2024-02-20", 2, 250.50, "completed"},
		{"2024-05-10", 3, 75.25, "pending"},
		{"2024-08-22", 1, 320.00, "completed"},
		{"2024-11-05", 4, 180.75, "shipped"},
		{"2025-01-08", 2, 420.00, "pending"},
		{"2025-06-15", 5, 95.50, "completed"},
		{"2025-09-20", 3, 550.25, "pending"},
		{"2026-01-10", 1, 200.00, "pending"},
		{"2026-03-05", 4, 175.50, "completed"},
	}

	for _, o := range orders {
		_, err := pool.Exec(ctx,
			"INSERT INTO orders (order_date, customer_id, amount, status) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
			o.orderDate, o.customerID, o.amount, o.status)
		if err != nil {
			log.Printf("Error inserting order: %v", err)
		}
	}

	fmt.Println("✅ Inserted orders across multiple quarters (2024-2026)")

	// Query orders from a specific partition
	rows, err := pool.Query(ctx,
		"SELECT id, order_date, customer_id, amount, status FROM orders WHERE order_date >= '2024-01-01' AND order_date < '2024-04-01'")
	if err != nil {
		log.Printf("Error querying orders: %v", err)
		return
	}
	defer rows.Close()

	fmt.Println("\n📋 Orders from Q1 2024 (partition: orders_2024_q1):")
	for rows.Next() {
		var id, customerID int
		var orderDate time.Time
		var amount float64
		var status string
		rows.Scan(&id, &orderDate, &customerID, &amount, &status)
		fmt.Printf("   ID: %d, Date: %s, Customer: %d, Amount: $%.2f, Status: %s\n",
			id, orderDate.Format("2006-01-02"), customerID, amount, status)
	}
}

func demonstrateHashPartitioning(ctx context.Context, pool *pgxpool.Pool) {
	// Insert users with different IDs (will be distributed across hash partitions)
	users := []struct {
		id       int
		username string
		email    string
	}{
		{1, "alice", "alice@example.com"},
		{2, "bob", "bob@example.com"},
		{3, "charlie", "charlie@example.com"},
		{4, "diana", "diana@example.com"},
		{5, "eve", "eve@example.com"},
		{6, "frank", "frank@example.com"},
		{7, "grace", "grace@example.com"},
		{8, "henry", "henry@example.com"},
	}

	for _, u := range users {
		_, err := pool.Exec(ctx,
			"INSERT INTO users (id, username, email) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
			u.id, u.username, u.email)
		if err != nil {
			log.Printf("Error inserting user: %v", err)
		}
	}

	fmt.Println("✅ Inserted users (distributed across 4 hash partitions)")

	// Show distribution across partitions
	partitions := []string{"users_p0", "users_p1", "users_p2", "users_p3"}
	fmt.Println("\n📋 User distribution across hash partitions:")
	for _, p := range partitions {
		var count int
		err := pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", p)).Scan(&count)
		if err != nil {
			log.Printf("Error counting %s: %v", p, err)
			continue
		}
		fmt.Printf("   %s: %d users\n", p, count)
	}
}

func demonstrateListPartitioning(ctx context.Context, pool *pgxpool.Pool) {
	// Insert products with different categories
	products := []struct {
		name     string
		category string
		price    float64
	}{
		{"iPhone 15", "phones", 999.00},
		{"MacBook Pro", "computers", 2499.00},
		{"Samsung TV", "electronics", 799.00},
		{"Nike Shoes", "shoes", 150.00},
		{"Leather Jacket", "clothing", 299.00},
		{"Dining Table", "furniture", 599.00},
		{"Blender", "kitchen", 89.00},
		{"Garden Hose", "garden", 45.00},
		{"Book: Go Programming", "books", 49.99},
	}

	for _, p := range products {
		_, err := pool.Exec(ctx,
			"INSERT INTO products (name, category, price) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
			p.name, p.category, p.price)
		if err != nil {
			log.Printf("Error inserting product: %v", err)
		}
	}

	fmt.Println("✅ Inserted products across category partitions")

	// Show distribution across partitions
	partitions := map[string]string{
		"products_electronics": "Electronics (electronics, computers, phones)",
		"products_clothing":    "Clothing (clothing, shoes, accessories)",
		"products_home":        "Home (furniture, kitchen, garden)",
		"products_other":       "Other (default partition)",
	}

	fmt.Println("\n📋 Product distribution across list partitions:")
	for table, desc := range partitions {
		var count int
		err := pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		if err != nil {
			log.Printf("Error counting %s: %v", table, err)
			continue
		}
		fmt.Printf("   %s: %d products\n", desc, count)
	}
}

func showPartitionStats(ctx context.Context, pool *pgxpool.Pool) {
	query := `
		SELECT 
			parent.relname AS parent_table,
			child.relname AS partition_name,
			pg_size_pretty(pg_relation_size(child.oid)) AS partition_size,
			(SELECT COUNT(*) FROM pg_class WHERE relname = child.relname) AS exists
		FROM pg_inherits
		JOIN pg_class parent ON pg_inherits.inhparent = parent.oid
		JOIN pg_class child ON pg_inherits.inhrelid = child.oid
		WHERE parent.relname IN ('orders', 'users', 'products')
		ORDER BY parent.relname, child.relname
	`

	rows, err := pool.Query(ctx, query)
	if err != nil {
		log.Printf("Error getting partition stats: %v", err)
		return
	}
	defer rows.Close()

	fmt.Println("📊 Partition Information:")
	currentParent := ""
	for rows.Next() {
		var parentTable, partitionName, partitionSize string
		var exists int
		rows.Scan(&parentTable, &partitionName, &partitionSize, &exists)

		if parentTable != currentParent {
			fmt.Printf("\n   Table: %s\n", parentTable)
			currentParent = parentTable
		}
		fmt.Printf("      └── %s (size: %s)\n", partitionName, partitionSize)
	}
}

func demonstratePartitionPruning(ctx context.Context, pool *pgxpool.Pool) {
	// Insert more data for performance testing
	statuses := []string{"pending", "completed", "shipped", "cancelled"}

	fmt.Println("📥 Inserting 1000 random orders for performance testing...")

	batch := &pgx.Batch{}
	for i := 0; i < 1000; i++ {
		// Random date between 2024-01-01 and 2026-03-31
		days := rand.IntN(820)
		orderDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
		customerID := rand.IntN(100) + 1
		amount := float64(rand.IntN(10000)) / 100.0
		status := statuses[rand.IntN(len(statuses))]

		batch.Queue(
			"INSERT INTO orders (order_date, customer_id, amount, status) VALUES ($1, $2, $3, $4)",
			orderDate, customerID, amount, status)
	}

	br := pool.SendBatch(ctx, batch)
	_, err := br.Exec()
	br.Close()
	if err != nil {
		log.Printf("Error batch inserting: %v", err)
	}

	fmt.Println("✅ Inserted 1000 orders")

	// Query with partition pruning (only scans relevant partition)
	fmt.Println("\n🔍 Query with partition pruning (Q1 2024 only):")
	start := time.Now()
	var count int
	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM orders WHERE order_date >= '2024-01-01' AND order_date < '2024-04-01'").Scan(&count)
	if err != nil {
		log.Printf("Error: %v", err)
		return
	}
	elapsed := time.Since(start)
	fmt.Printf("   Found %d orders in Q1 2024 (took %v)\n", count, elapsed)

	// Show EXPLAIN ANALYZE for partition pruning
	fmt.Println("\n📋 EXPLAIN output showing partition pruning:")
	rows, err := pool.Query(ctx,
		"EXPLAIN SELECT * FROM orders WHERE order_date >= '2024-01-01' AND order_date < '2024-04-01'")
	if err != nil {
		log.Printf("Error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var plan string
		rows.Scan(&plan)
		fmt.Printf("   %s\n", plan)
	}
}
