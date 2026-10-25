package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Denormalization stores redundant, derived data next to where it's read so hot
// queries skip joins and aggregates. Here `orders` carries copies of the customer's
// name plus the item count and total computed from order_items.
//
// This program shows:
//   - read speedups for an order list and a revenue dashboard
//   - triggers keeping the copies consistent on inserts and updates
//   - the write cost: renaming one customer rewrites thousands of orders
//   - drift when a code path bypasses the sync logic, and a reconciliation job
//   - the application-level alternative: update the copy in the same transaction

// Same answers, two ways. Normalized: join + aggregate on every read.
const listNormalized = `
	SELECT o.id, c.name, count(*), sum(i.qty * i.unit_price)
	FROM orders o
	JOIN customers c   ON c.id = o.customer_id
	JOIN order_items i ON i.order_id = o.id
	WHERE o.customer_id = 1
	GROUP BY o.id, c.name, o.created_at
	ORDER BY o.created_at DESC LIMIT 50`

// Denormalized: everything is already on the order row.
const listDenormalized = `
	SELECT id, customer_name, item_count, total_amount
	FROM orders
	WHERE customer_id = 1
	ORDER BY created_at DESC LIMIT 50`

const dashNormalized = `
	SELECT c.name, sum(i.qty * i.unit_price) AS revenue
	FROM orders o
	JOIN customers c   ON c.id = o.customer_id
	JOIN order_items i ON i.order_id = o.id
	WHERE o.created_at > now() - interval '30 days'
	GROUP BY c.name ORDER BY revenue DESC LIMIT 10`

const dashDenormalized = `
	SELECT customer_name, sum(total_amount) AS revenue
	FROM orders
	WHERE created_at > now() - interval '30 days'
	GROUP BY customer_name ORDER BY revenue DESC LIMIT 10`

// Rows whose redundant columns disagree with the source of truth.
const driftQuery = `
	SELECT o.id
	FROM orders o
	JOIN customers c ON c.id = o.customer_id
	LEFT JOIN (SELECT order_id, count(*) AS n, sum(qty * unit_price) AS total
	           FROM order_items GROUP BY order_id) t ON t.order_id = o.id
	WHERE o.customer_name IS DISTINCT FROM c.name
	   OR o.item_count   <> coalesce(t.n, 0)
	   OR o.total_amount <> coalesce(t.total, 0)`

type App struct{ db *pgxpool.Pool }

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5521/shop?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	app.reset(ctx)
	fmt.Println("✅ Connected to shop (5521) — 200k orders, ~600k order items")

	banner("🧬 DENORMALIZATION SIMULATION")

	section("⚡ READS — 'latest 50 orders of a customer' with name, item count, total")
	app.compare(ctx, listNormalized, listDenormalized)

	section("⚡ READS — 'top 10 customers by revenue, last 30 days'")
	app.compare(ctx, dashNormalized, dashDenormalized)

	section("🔁 CONSISTENCY — triggers maintain the copies automatically")
	orderID := app.placeOrder(ctx, 42, map[int64]int{1: 2, 2: 1, 3: 5})
	app.showOrder(ctx, orderID, "new order (3 items)")
	app.exec(ctx, "UPDATE order_items SET qty = qty + 10 WHERE order_id = $1 AND product_id = 1", orderID)
	app.showOrder(ctx, orderID, "after qty += 10 on one line")
	app.exec(ctx, "DELETE FROM order_items WHERE order_id = $1 AND product_id = 3", orderID)
	app.showOrder(ctx, orderID, "after deleting one line")

	section("✍️  WRITE COST — renaming one customer")
	app.timeExec(ctx, "rename customer 2 (≈100 orders)", "UPDATE customers SET name = 'Globex Corp' WHERE id = 2")
	app.timeExec(ctx, "rename customer 1 (10,000 orders)", "UPDATE customers SET name = 'Megacorp Ltd' WHERE id = 1")
	fmt.Println("   normalized: 1 row changes. denormalized: the trigger also rewrites every")
	fmt.Println("   copy — one logical change, thousands of physical writes (and WAL, locks)")

	section("🧟 DRIFT — a code path that bypasses the sync logic")
	// e.g. a bulk import, a hotfix script, or a replica of the data in another
	// service. Simulate it by disabling the item trigger for one write.
	app.exec(ctx, "ALTER TABLE order_items DISABLE TRIGGER order_items_ins")
	app.exec(ctx, `INSERT INTO order_items (order_id, product_id, qty, unit_price)
		SELECT id, 499, 1, 9.99 FROM orders WHERE customer_id = 7
		ON CONFLICT DO NOTHING`)
	app.exec(ctx, "ALTER TABLE order_items ENABLE TRIGGER order_items_ins")
	drifted := app.countDrift(ctx)
	fmt.Printf("   reconciliation query finds %d orders whose totals are wrong ❌\n", drifted)

	section("🧹 RECONCILE — recompute copies from the source of truth")
	start := time.Now()
	tag, err := db.Exec(ctx, `
		UPDATE orders o SET item_count = t.n, total_amount = t.total
		FROM (SELECT order_id, count(*) AS n, sum(qty * unit_price) AS total
		      FROM order_items GROUP BY order_id) t
		WHERE t.order_id = o.id
		  AND (o.item_count <> t.n OR o.total_amount <> t.total)`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   repaired %d orders in %v; drift now: %d ✅\n",
		tag.RowsAffected(), time.Since(start).Round(time.Millisecond), app.countDrift(ctx))

	section("🧑‍💻 APP-LEVEL ALTERNATIVE — update the copy in the same transaction")
	app.appLevelAddItem(ctx, orderID, 4, 3)
	app.showOrder(ctx, orderID, "after app-level add (trigger off)")
	fmt.Printf("   drift: %d — consistent, but EVERY writer must remember to do this\n", app.countDrift(ctx))

	section("📊 SUMMARY")
	fmt.Println("   Denormalize what's read constantly and changes rarely (names, counts,")
	fmt.Println("   totals). Keep the normalized data as the source of truth, sync copies in")
	fmt.Println("   the database (triggers) or one code path, and run reconciliation checks.")
}

// compare runs both queries a few times and reports the best time of each.
func (a *App) compare(ctx context.Context, normalized, denormalized string) {
	n := a.best(ctx, normalized)
	d := a.best(ctx, denormalized)
	fmt.Printf("   normalized   (joins + GROUP BY): %8.2f ms\n", ms(n))
	fmt.Printf("   denormalized (single table):     %8.2f ms   → %.0fx faster\n", ms(d), float64(n)/float64(d))
}

func (a *App) best(ctx context.Context, sql string) time.Duration {
	best := time.Hour
	for range 5 {
		start := time.Now()
		rows, err := a.db.Query(ctx, sql)
		if err != nil {
			log.Fatalf("query: %v", err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			log.Fatal(err)
		}
		rows.Close()
		best = min(best, time.Since(start))
	}
	return best
}

// placeOrder inserts an order and its items. The application writes only normalized
// data; triggers fill customer_name, item_count and total_amount.
func (a *App) placeOrder(ctx context.Context, customer int64, items map[int64]int) int64 {
	var id int64
	err := pgx.BeginFunc(ctx, a.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO orders (customer_id) VALUES ($1) RETURNING id", customer).Scan(&id); err != nil {
			return err
		}
		for product, qty := range items {
			if _, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, product_id, qty, unit_price)
				SELECT $1, id, $2, price FROM products WHERE id = $3`, id, qty, product); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatalf("place order: %v", err)
	}
	return id
}

// appLevelAddItem is the no-trigger approach: the writer updates the derived columns
// itself, atomically with the source change.
func (a *App) appLevelAddItem(ctx context.Context, orderID, product int64, qty int) {
	a.exec(ctx, "ALTER TABLE order_items DISABLE TRIGGER order_items_ins")
	defer a.exec(ctx, "ALTER TABLE order_items ENABLE TRIGGER order_items_ins")
	err := pgx.BeginFunc(ctx, a.db, func(tx pgx.Tx) error {
		var price string // keep NUMERIC exact: carry it as text, never float
		if err := tx.QueryRow(ctx, "SELECT price::text FROM products WHERE id = $1", product).Scan(&price); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, product_id, qty, unit_price)
			VALUES ($1, $2, $3, $4::numeric)`, orderID, product, qty, price); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE orders SET item_count = item_count + 1,
			total_amount = total_amount + $2::int * $3::numeric WHERE id = $1`, orderID, qty, price)
		return err
	})
	if err != nil {
		log.Fatalf("app-level add: %v", err)
	}
}

func (a *App) showOrder(ctx context.Context, id int64, label string) {
	var name string
	var count int
	var total string
	if err := a.db.QueryRow(ctx,
		"SELECT customer_name, item_count, total_amount::text FROM orders WHERE id = $1", id).Scan(&name, &count, &total); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   %-34s customer_name=%q item_count=%d total=%s\n", label+":", name, count, total)
}

func (a *App) timeExec(ctx context.Context, label, sql string) {
	start := time.Now()
	a.exec(ctx, sql)
	fmt.Printf("   %-34s %8.2f ms\n", label, ms(time.Since(start)))
}

func (a *App) countDrift(ctx context.Context) int {
	var n int
	if err := a.db.QueryRow(ctx, "SELECT count(*) FROM ("+driftQuery+") d").Scan(&n); err != nil {
		log.Fatal(err)
	}
	return n
}

// reset undoes earlier runs: demo orders/items and renamed customers.
func (a *App) reset(ctx context.Context) {
	a.exec(ctx, "DELETE FROM order_items WHERE product_id = 499 AND unit_price = 9.99")
	a.exec(ctx, "DELETE FROM orders WHERE customer_id = 42 AND id > 200000")
	a.exec(ctx, "UPDATE customers SET name = 'Customer ' || id WHERE id IN (1, 2)")
	a.exec(ctx, "ALTER TABLE order_items ENABLE TRIGGER USER")
}

func (a *App) exec(ctx context.Context, sql string, args ...any) {
	if _, err := a.db.Exec(ctx, sql, args...); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}
