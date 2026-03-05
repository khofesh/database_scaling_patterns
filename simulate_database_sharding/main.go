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

// ShardConfig holds configuration for a single shard
type ShardConfig struct {
	Name string
	URL  string
}

// ShardManager manages connections to multiple database shards
type ShardManager struct {
	shards []*pgxpool.Pool
	count  int
}

// NewShardManager creates a new shard manager with connections to all shards
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

// Close closes all shard connections
func (sm *ShardManager) Close() {
	for _, pool := range sm.shards {
		pool.Close()
	}
}

// GetShardIndex returns the shard index for a given user ID using consistent hashing
func (sm *ShardManager) GetShardIndex(userID int) int {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprintf("%d", userID)))
	return int(h.Sum32()) % sm.count
}

// GetShard returns the connection pool for a given user ID
func (sm *ShardManager) GetShard(userID int) *pgxpool.Pool {
	return sm.shards[sm.GetShardIndex(userID)]
}

// GetShardByIndex returns the connection pool for a specific shard index
func (sm *ShardManager) GetShardByIndex(index int) *pgxpool.Pool {
	return sm.shards[index]
}

// GetAllShards returns all shard connections for scatter-gather queries
func (sm *ShardManager) GetAllShards() []*pgxpool.Pool {
	return sm.shards
}

// User represents a user entity
type User struct {
	ID        int
	Username  string
	Email     string
	CreatedAt time.Time
}

// Order represents an order entity
type Order struct {
	ID        int
	UserID    int
	OrderDate time.Time
	Amount    float64
	Status    string
	CreatedAt time.Time
}

func main() {
	ctx := context.Background()

	// Define shard configurations
	shardConfigs := []ShardConfig{
		{Name: "Shard 1", URL: "postgres://postgres:postgres@localhost:5433/shard_db?sslmode=disable"},
		{Name: "Shard 2", URL: "postgres://postgres:postgres@localhost:5434/shard_db?sslmode=disable"},
		{Name: "Shard 3", URL: "postgres://postgres:postgres@localhost:5435/shard_db?sslmode=disable"},
	}

	// Create shard manager
	sm, err := NewShardManager(ctx, shardConfigs)
	if err != nil {
		log.Fatalf("Failed to initialize shard manager: %v", err)
	}
	defer sm.Close()

	fmt.Println("\n" + string(make([]byte, 60)))
	fmt.Println("🔀 DATABASE SHARDING SIMULATION")
	fmt.Println(string(make([]byte, 60)))

	// Demonstrate user sharding
	fmt.Println("\n👥 USER SHARDING DEMO")
	fmt.Println("-" + string(make([]byte, 50)))
	demonstrateUserSharding(ctx, sm)

	// Demonstrate order sharding (co-located with users)
	fmt.Println("\n📦 ORDER SHARDING DEMO (Co-located with Users)")
	fmt.Println("-" + string(make([]byte, 50)))
	demonstrateOrderSharding(ctx, sm)

	// Demonstrate scatter-gather query
	fmt.Println("\n🔍 SCATTER-GATHER QUERY DEMO")
	fmt.Println("-" + string(make([]byte, 50)))
	demonstrateScatterGather(ctx, sm)

	// Show shard statistics
	fmt.Println("\n📊 SHARD STATISTICS")
	fmt.Println("-" + string(make([]byte, 50)))
	showShardStats(ctx, sm)

	// Demonstrate cross-shard aggregation
	fmt.Println("\n📈 CROSS-SHARD AGGREGATION")
	fmt.Println("-" + string(make([]byte, 50)))
	demonstrateCrossShardAggregation(ctx, sm)
}

func demonstrateUserSharding(ctx context.Context, sm *ShardManager) {
	users := []User{
		{ID: 1, Username: "alice", Email: "alice@example.com"},
		{ID: 2, Username: "bob", Email: "bob@example.com"},
		{ID: 3, Username: "charlie", Email: "charlie@example.com"},
		{ID: 4, Username: "diana", Email: "diana@example.com"},
		{ID: 5, Username: "eve", Email: "eve@example.com"},
		{ID: 6, Username: "frank", Email: "frank@example.com"},
		{ID: 7, Username: "grace", Email: "grace@example.com"},
		{ID: 8, Username: "henry", Email: "henry@example.com"},
		{ID: 9, Username: "ivy", Email: "ivy@example.com"},
		{ID: 10, Username: "jack", Email: "jack@example.com"},
	}

	fmt.Println("📥 Inserting users across shards...")
	for _, u := range users {
		shardIdx := sm.GetShardIndex(u.ID)
		shard := sm.GetShard(u.ID)

		_, err := shard.Exec(ctx,
			"INSERT INTO users (id, username, email) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING",
			u.ID, u.Username, u.Email)
		if err != nil {
			log.Printf("Error inserting user %s: %v", u.Username, err)
			continue
		}
		fmt.Printf("   User %s (ID: %d) → Shard %d\n", u.Username, u.ID, shardIdx+1)
	}

	// Demonstrate reading a user from the correct shard
	fmt.Println("\n📖 Reading user by ID (direct shard access):")
	userID := 5
	shard := sm.GetShard(userID)
	var username, email string
	err := shard.QueryRow(ctx, "SELECT username, email FROM users WHERE id = $1", userID).Scan(&username, &email)
	if err != nil {
		log.Printf("Error reading user: %v", err)
	} else {
		fmt.Printf("   Found user ID %d on Shard %d: %s (%s)\n", userID, sm.GetShardIndex(userID)+1, username, email)
	}
}

func demonstrateOrderSharding(ctx context.Context, sm *ShardManager) {
	rand.Seed(time.Now().UnixNano())
	statuses := []string{"pending", "completed", "shipped", "cancelled"}

	fmt.Println("📥 Inserting orders (co-located with their users)...")

	// Insert orders for various users
	for userID := 1; userID <= 10; userID++ {
		shard := sm.GetShard(userID)
		shardIdx := sm.GetShardIndex(userID)

		// Each user gets 2-5 orders
		numOrders := rand.Intn(4) + 2
		for i := 0; i < numOrders; i++ {
			orderDate := time.Now().AddDate(0, 0, -rand.Intn(365))
			amount := float64(rand.Intn(50000)) / 100.0
			status := statuses[rand.Intn(len(statuses))]

			_, err := shard.Exec(ctx,
				"INSERT INTO orders (user_id, order_date, amount, status) VALUES ($1, $2, $3, $4)",
				userID, orderDate, amount, status)
			if err != nil {
				log.Printf("Error inserting order for user %d: %v", userID, err)
			}
		}
		fmt.Printf("   User %d: %d orders → Shard %d\n", userID, numOrders, shardIdx+1)
	}

	// Demonstrate reading orders for a specific user (single shard query)
	fmt.Println("\n📖 Reading orders for user ID 3 (single shard query):")
	userID := 3
	shard := sm.GetShard(userID)
	rows, err := shard.Query(ctx,
		"SELECT id, order_date, amount, status FROM orders WHERE user_id = $1 ORDER BY order_date DESC LIMIT 3",
		userID)
	if err != nil {
		log.Printf("Error reading orders: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var orderDate time.Time
		var amount float64
		var status string
		rows.Scan(&id, &orderDate, &amount, &status)
		fmt.Printf("   Order #%d: %s, $%.2f, %s\n", id, orderDate.Format("2006-01-02"), amount, status)
	}
}

func demonstrateScatterGather(ctx context.Context, sm *ShardManager) {
	fmt.Println("🔍 Searching for user 'alice' across all shards...")

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

			var user User
			err := pool.QueryRow(ctx,
				"SELECT id, username, email FROM users WHERE username = $1", "alice").
				Scan(&user.ID, &user.Username, &user.Email)

			if err == nil {
				results <- struct {
					shardIdx int
					user     *User
				}{idx, &user}
			}
		}(i, shard)
	}

	// Wait for all queries to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	found := false
	for result := range results {
		fmt.Printf("   ✅ Found on Shard %d: %s (ID: %d, Email: %s)\n",
			result.shardIdx+1, result.user.Username, result.user.ID, result.user.Email)
		found = true
	}

	if !found {
		fmt.Println("   ❌ User not found on any shard")
	}
}

func showShardStats(ctx context.Context, sm *ShardManager) {
	fmt.Println("📊 Data distribution across shards:")

	var totalUsers, totalOrders int

	for i, shard := range sm.GetAllShards() {
		var userCount, orderCount int

		err := shard.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&userCount)
		if err != nil {
			log.Printf("Error counting users on shard %d: %v", i+1, err)
		}

		err = shard.QueryRow(ctx, "SELECT COUNT(*) FROM orders").Scan(&orderCount)
		if err != nil {
			log.Printf("Error counting orders on shard %d: %v", i+1, err)
		}

		totalUsers += userCount
		totalOrders += orderCount

		fmt.Printf("\n   Shard %d:\n", i+1)
		fmt.Printf("      Users:  %d\n", userCount)
		fmt.Printf("      Orders: %d\n", orderCount)
	}

	fmt.Printf("\n   📈 Total across all shards:\n")
	fmt.Printf("      Users:  %d\n", totalUsers)
	fmt.Printf("      Orders: %d\n", totalOrders)
}

func demonstrateCrossShardAggregation(ctx context.Context, sm *ShardManager) {
	fmt.Println("📈 Aggregating order statistics across all shards...")

	type ShardStats struct {
		shardIdx   int
		totalSales float64
		orderCount int
		avgOrder   float64
	}

	var wg sync.WaitGroup
	results := make(chan ShardStats, sm.count)

	// Query all shards in parallel
	start := time.Now()
	for i, shard := range sm.GetAllShards() {
		wg.Add(1)
		go func(idx int, pool *pgxpool.Pool) {
			defer wg.Done()

			var stats ShardStats
			stats.shardIdx = idx

			err := pool.QueryRow(ctx, `
				SELECT 
					COALESCE(SUM(amount), 0) as total_sales,
					COUNT(*) as order_count,
					COALESCE(AVG(amount), 0) as avg_order
				FROM orders
			`).Scan(&stats.totalSales, &stats.orderCount, &stats.avgOrder)

			if err != nil {
				log.Printf("Error getting stats from shard %d: %v", idx+1, err)
				return
			}

			results <- stats
		}(i, shard)
	}

	// Wait for all queries to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Aggregate results
	var globalTotalSales float64
	var globalOrderCount int
	shardStats := make([]ShardStats, 0, sm.count)

	for stats := range results {
		shardStats = append(shardStats, stats)
		globalTotalSales += stats.totalSales
		globalOrderCount += stats.orderCount
	}

	elapsed := time.Since(start)

	// Display per-shard stats
	for _, stats := range shardStats {
		fmt.Printf("\n   Shard %d:\n", stats.shardIdx+1)
		fmt.Printf("      Total Sales: $%.2f\n", stats.totalSales)
		fmt.Printf("      Order Count: %d\n", stats.orderCount)
		fmt.Printf("      Avg Order:   $%.2f\n", stats.avgOrder)
	}

	// Display global aggregation
	var globalAvg float64
	if globalOrderCount > 0 {
		globalAvg = globalTotalSales / float64(globalOrderCount)
	}

	fmt.Printf("\n   🌍 Global Aggregation (took %v):\n", elapsed)
	fmt.Printf("      Total Sales: $%.2f\n", globalTotalSales)
	fmt.Printf("      Total Orders: %d\n", globalOrderCount)
	fmt.Printf("      Global Avg Order: $%.2f\n", globalAvg)
}
