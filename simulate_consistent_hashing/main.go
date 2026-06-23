package main

import (
	"context"
	"fmt"
	"hash/crc32"
	"log"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HashRing is a consistent-hashing router. Each physical node is placed at many
// points on a 32-bit ring via "virtual nodes" (vnodes). A key is hashed onto the
// same ring and owned by the first node found walking clockwise. Adding or
// removing a node only re-homes the keys that fall in the affected arcs — not the
// whole keyspace, which is what modulo sharding would do.
type HashRing struct {
	mu       sync.RWMutex
	replicas int               // vnodes per physical node
	ring     []uint32          // sorted hashes of every vnode
	owners   map[uint32]string // vnode hash -> physical node name
	nodes    map[string]bool   // set of physical nodes currently on the ring
}

func NewHashRing(replicas int) *HashRing {
	return &HashRing{
		replicas: replicas,
		owners:   make(map[uint32]string),
		nodes:    make(map[string]bool),
	}
}

func hashKey(s string) uint32 {
	return crc32.ChecksumIEEE([]byte(s))
}

// vnodeKey gives each virtual node a distinct point on the ring.
func vnodeKey(node string, i int) string {
	return fmt.Sprintf("%s#%d", node, i)
}

func (h *HashRing) AddNode(node string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nodes[node] {
		return
	}
	h.nodes[node] = true
	for i := 0; i < h.replicas; i++ {
		hash := hashKey(vnodeKey(node, i))
		h.ring = append(h.ring, hash)
		h.owners[hash] = node
	}
	sort.Slice(h.ring, func(i, j int) bool { return h.ring[i] < h.ring[j] })
}

func (h *HashRing) RemoveNode(node string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.nodes[node] {
		return
	}
	delete(h.nodes, node)
	kept := h.ring[:0]
	for _, hash := range h.ring {
		if h.owners[hash] == node {
			delete(h.owners, hash)
			continue
		}
		kept = append(kept, hash)
	}
	h.ring = kept
}

// Get returns the node that owns a key: the first vnode clockwise from the key's
// hash, wrapping around the end of the ring.
func (h *HashRing) Get(key string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.ring) == 0 {
		return ""
	}
	hash := hashKey(key)
	idx := sort.Search(len(h.ring), func(i int) bool { return h.ring[i] >= hash })
	if idx == len(h.ring) {
		idx = 0 // wrap around
	}
	return h.owners[h.ring[idx]]
}

func main() {
	ctx := context.Background()

	// Physical nodes and their connection strings. node4 starts offline (in the
	// ring sense) so we can add it mid-demo.
	dsn := map[string]string{
		"node1": "postgres://postgres:postgres@localhost:5450/chash_db?sslmode=disable",
		"node2": "postgres://postgres:postgres@localhost:5451/chash_db?sslmode=disable",
		"node3": "postgres://postgres:postgres@localhost:5452/chash_db?sslmode=disable",
		"node4": "postgres://postgres:postgres@localhost:5453/chash_db?sslmode=disable",
	}
	pools := make(map[string]*pgxpool.Pool)
	for name, url := range dsn {
		p, err := pgxpool.New(ctx, url)
		if err != nil {
			log.Fatalf("connect %s: %v", name, err)
		}
		defer p.Close()
		pools[name] = p
	}
	fmt.Println("✅ Connected to 4 PostgreSQL nodes")

	const vnodes = 1000
	ring := NewHashRing(vnodes)
	ring.AddNode("node1")
	ring.AddNode("node2")
	ring.AddNode("node3")

	banner("🔗 CONSISTENT HASHING SIMULATION")

	// Generate a fixed set of keys and record where each one currently belongs.
	keys := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		keys = append(keys, fmt.Sprintf("user:%d", i))
	}

	section(fmt.Sprintf("📦 INITIAL PLACEMENT — 3 nodes, %d vnodes each", vnodes))
	before := placement(ring, keys)
	printDistribution(before, []string{"node1", "node2", "node3"})

	// Actually persist each key on its owning node so the ring isn't just theory.
	// Clear first so re-runs reflect the current placement, not accumulated rows.
	section("💾 WRITING KEYS TO THEIR OWNING NODES")
	for name, p := range pools {
		if _, err := p.Exec(ctx, "TRUNCATE kv"); err != nil {
			log.Printf("truncate %s: %v", name, err)
		}
	}
	for _, k := range keys {
		owner := ring.Get(k)
		_, err := pools[owner].Exec(ctx,
			"INSERT INTO kv (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING",
			k, owner)
		if err != nil {
			log.Printf("write %s: %v", k, err)
		}
	}
	verifyCounts(ctx, pools, []string{"node1", "node2", "node3"})

	// Add a 4th node and measure churn.
	section("➕ ADDING node4 — consistent hashing")
	ring.AddNode("node4")
	after := placement(ring, keys)
	moved := countMoved(before, after)
	printDistribution(after, []string{"node1", "node2", "node3", "node4"})
	fmt.Printf("\n   Keys re-homed: %d / %d  (%.1f%%)\n", moved, len(keys),
		100*float64(moved)/float64(len(keys)))
	fmt.Printf("   Ideal for 3→4 nodes: ~%.1f%% (1/4 of keys)\n", 100.0/4)

	// Contrast with naive modulo sharding, which reshuffles almost everything.
	section("⚖️  COMPARISON — modulo sharding (key % N)")
	modBefore := moduloPlacement(keys, 3)
	modAfter := moduloPlacement(keys, 4)
	modMoved := countMoved(modBefore, modAfter)
	fmt.Printf("   Modulo 3→4 re-homed: %d / %d  (%.1f%%)\n", modMoved, len(keys),
		100*float64(modMoved)/float64(len(keys)))
	fmt.Printf("   Consistent hash:     %d / %d  (%.1f%%)\n", moved, len(keys),
		100*float64(moved)/float64(len(keys)))
	fmt.Println("   → modulo moves most keys; consistent hashing moves ~1/N.")

	section("📊 SUMMARY")
	fmt.Println("   Virtual nodes smooth out the per-node load imbalance.")
	fmt.Println("   Adding/removing a node only disturbs neighboring arcs of the ring.")
}

// placement returns key -> owning node for the current ring.
func placement(ring *HashRing, keys []string) map[string]string {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		m[k] = ring.Get(k)
	}
	return m
}

func moduloPlacement(keys []string, n int) map[string]string {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		m[k] = fmt.Sprintf("node%d", int(hashKey(k))%n+1)
	}
	return m
}

func countMoved(before, after map[string]string) int {
	moved := 0
	for k, b := range before {
		if after[k] != b {
			moved++
		}
	}
	return moved
}

func printDistribution(p map[string]string, nodes []string) {
	counts := map[string]int{}
	for _, node := range p {
		counts[node]++
	}
	total := len(p)
	for _, n := range nodes {
		c := counts[n]
		bar := ""
		for i := 0; i < c*40/total; i++ {
			bar += "█"
		}
		fmt.Printf("   %-6s %4d keys  %s %.1f%%\n", n, c, bar, 100*float64(c)/float64(total))
	}
}

func verifyCounts(ctx context.Context, pools map[string]*pgxpool.Pool, nodes []string) {
	for _, n := range nodes {
		var count int
		if err := pools[n].QueryRow(ctx, "SELECT COUNT(*) FROM kv").Scan(&count); err != nil {
			log.Printf("count %s: %v", n, err)
			continue
		}
		fmt.Printf("   %-6s stored %d rows\n", n, count)
	}
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}
