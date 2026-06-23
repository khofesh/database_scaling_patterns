package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Data tiering keeps hot (recent, frequently-read) data on fast storage and pushes
// aging data down to cheaper warm/cold storage — automatically and by age — without
// the application having to know where any row physically lives. The whole table is
// still queried through one parent; PostgreSQL prunes to the right partition(s).
//
// Mechanics used here:
//   - monthly RANGE partitions of `events`
//   - three tablespaces (hot_tier / warm_tier / cold_tier) as the storage tiers
//   - a tiering job that moves whole partitions between tablespaces as they age
//   - partition pruning so a "recent data" query only touches the hot tier

// tierFor maps a partition's age (in whole months) to a storage tier.
func tierFor(ageMonths int) string {
	switch {
	case ageMonths <= 0:
		return "hot_tier" // current month: fast storage
	case ageMonths <= 2:
		return "warm_tier" // 1–2 months old: mid storage
	default:
		return "cold_tier" // older: cheap/archival storage
	}
}

type App struct{ db *pgxpool.Pool }

func main() {
	ctx := context.Background()

	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5491/tiering?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	fmt.Println("✅ Connected to tiered store (5491)")

	banner("🌡️  DATA TIERING SIMULATION (hot / warm / cold)")

	// Work from the first day of the current month so partition boundaries are clean.
	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	section("🧱 PARTITIONS — one month per partition, newest on hot storage")
	// Create 5 monthly partitions: current month and the four before it.
	months := make([]time.Time, 0, 5)
	for i := 4; i >= 0; i-- {
		months = append(months, thisMonth.AddDate(0, -i, 0))
	}
	for _, m := range months {
		app.ensurePartition(ctx, m)
	}
	fmt.Printf("   Created %d monthly partitions, all initially on hot_tier\n", len(months))

	section("📥 INGEST — drop sample rows into each month")
	app.seed(ctx, months)
	app.printPlacement(ctx)

	section("⬇️  TIERING JOB — age partitions down by access pattern")
	fmt.Println("   policy: current→hot, 1–2 months→warm, older→cold")
	app.runTiering(ctx, thisMonth)
	app.printPlacement(ctx)

	section("✂️  PARTITION PRUNING — a 'recent data' query only hits hot storage")
	app.explainRecent(ctx, thisMonth)

	section("🔎 TRANSPARENT QUERY — one table, all tiers, no tier awareness")
	var total int
	must(db.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&total))
	fmt.Printf("   SELECT count(*) FROM events → %d rows across hot+warm+cold\n", total)

	section("📊 SUMMARY")
	fmt.Println("   Hot data stays on fast storage; aging partitions migrate to cheaper")
	fmt.Println("   tiers automatically. Queries stay simple; pruning keeps them fast.")
}

// ensurePartition creates the monthly partition for month m on the hot tier if it
// does not already exist. New data always arrives hot; the tiering job demotes it.
func (a *App) ensurePartition(ctx context.Context, m time.Time) {
	name := partName(m)
	start := m.Format("2006-01-02")
	end := m.AddDate(0, 1, 0).Format("2006-01-02")
	sql := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s
		PARTITION OF events FOR VALUES FROM ('%s') TO ('%s') TABLESPACE hot_tier`,
		name, start, end)
	if _, err := a.db.Exec(ctx, sql); err != nil {
		log.Fatalf("create partition %s: %v", name, err)
	}
}

// seed clears and inserts a few rows per month so each tier ends up with data.
func (a *App) seed(ctx context.Context, months []time.Time) {
	if _, err := a.db.Exec(ctx, "TRUNCATE events"); err != nil {
		log.Fatalf("truncate: %v", err)
	}
	for _, m := range months {
		// place rows mid-month so they land squarely inside the partition range
		ts := m.AddDate(0, 0, 14)
		for i := range 3 {
			if _, err := a.db.Exec(ctx,
				"INSERT INTO events (created_at, payload) VALUES ($1, $2)",
				ts, fmt.Sprintf("event in %s #%d", m.Format("2006-01"), i+1)); err != nil {
				log.Fatalf("insert: %v", err)
			}
		}
	}
	fmt.Printf("   Inserted 3 rows into each of %d months\n", len(months))
}

// runTiering walks every partition, computes its age, and moves it to the tablespace
// its age maps to. Moving a partition's storage is a single ALTER TABLE — the data
// is physically relocated to the tier's directory, transparently to readers.
func (a *App) runTiering(ctx context.Context, thisMonth time.Time) {
	rows, err := a.db.Query(ctx, `
		SELECT child.relname
		FROM pg_inherits
		JOIN pg_class parent ON parent.oid = pg_inherits.inhparent
		JOIN pg_class child  ON child.oid  = pg_inherits.inhrelid
		WHERE parent.relname = 'events'
		ORDER BY child.relname`)
	if err != nil {
		log.Fatalf("list partitions: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()

	for _, name := range names {
		m, ok := monthFromPart(name)
		if !ok {
			continue
		}
		ageMonths := monthsBetween(m, thisMonth)
		target := tierFor(ageMonths)
		if _, err := a.db.Exec(ctx,
			fmt.Sprintf("ALTER TABLE %s SET TABLESPACE %s", name, target)); err != nil {
			log.Fatalf("move %s → %s: %v", name, target, err)
		}
		fmt.Printf("   %s (age %dmo) → %s\n", name, ageMonths, target)
	}
}

// printPlacement shows, per partition, which tier it sits on and how many rows it
// holds — the observable result of tiering.
func (a *App) printPlacement(ctx context.Context) {
	rows, err := a.db.Query(ctx, `
		SELECT child.relname,
		       COALESCE(ts.spcname, 'pg_default') AS tablespace
		FROM pg_inherits
		JOIN pg_class parent     ON parent.oid = pg_inherits.inhparent
		JOIN pg_class child      ON child.oid  = pg_inherits.inhrelid
		LEFT JOIN pg_tablespace ts ON ts.oid = child.reltablespace
		WHERE parent.relname = 'events'
		ORDER BY child.relname`)
	if err != nil {
		log.Fatalf("placement: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, tablespace string
		rows.Scan(&name, &tablespace)
		var n int
		a.db.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", name)).Scan(&n)
		fmt.Printf("   %-16s %-10s %d rows\n", name, tablespace, n)
	}
}

// explainRecent shows that filtering to the current month prunes away the cold/warm
// partitions: the plan only scans the hot partition.
func (a *App) explainRecent(ctx context.Context, thisMonth time.Time) {
	rows, err := a.db.Query(ctx,
		"EXPLAIN SELECT count(*) FROM events WHERE created_at >= $1", thisMonth)
	if err != nil {
		log.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	hot := partName(thisMonth)
	for rows.Next() {
		var line string
		rows.Scan(&line)
		marker := "  "
		if strings.Contains(line, hot) {
			marker = "→ " // the only partition the planner kept
		}
		fmt.Printf("   %s%s\n", marker, strings.TrimSpace(line))
	}
	fmt.Printf("   Only %s (hot) is scanned; warm+cold partitions are pruned.\n", hot)
}

// --- helpers ------------------------------------------------------------------

func partName(m time.Time) string { return "events_" + m.Format("2006_01") }

func monthFromPart(name string) (time.Time, bool) {
	const prefix = "events_"
	if !strings.HasPrefix(name, prefix) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006_01", strings.TrimPrefix(name, prefix))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func monthsBetween(older, newer time.Time) int {
	return int(newer.Year()-older.Year())*12 + int(newer.Month()) - int(older.Month())
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}
