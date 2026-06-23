package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CQRS = Command Query Responsibility Segregation. Writes go through the
// CommandHandler into a normalized WRITE store; reads come from a denormalized
// READ store. The two are kept in sync asynchronously by the Projector, which
// consumes events from the write store's outbox. Reads are therefore eventually
// consistent: a fresh write is invisible to queries until the projector runs.

type Item struct {
	Product    string `json:"product"`
	Qty        int    `json:"qty"`
	PriceCents int    `json:"price_cents"`
}

type CQRS struct {
	write *pgxpool.Pool // normalized source of truth
	read  *pgxpool.Pool // denormalized query model
}

// --- COMMAND side -------------------------------------------------------------

// PlaceOrder is a command: it mutates the write store transactionally and, in the
// SAME transaction, appends an event to the outbox. Atomicity here is the whole
// point — the read model can never miss an update that the write model committed.
func (c *CQRS) PlaceOrder(ctx context.Context, customerID int, items []Item) (int, error) {
	tx, err := c.write.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var orderID int
	if err := tx.QueryRow(ctx,
		"INSERT INTO orders (customer_id) VALUES ($1) RETURNING id",
		customerID).Scan(&orderID); err != nil {
		return 0, err
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx,
			"INSERT INTO order_items (order_id, product, qty, price_cents) VALUES ($1, $2, $3, $4)",
			orderID, it.Product, it.Qty, it.PriceCents); err != nil {
			return 0, err
		}
	}
	if err := appendEvent(ctx, tx, "order_placed", orderID); err != nil {
		return 0, err
	}
	return orderID, tx.Commit(ctx)
}

// UpdateStatus is another command — same outbox pattern.
func (c *CQRS) UpdateStatus(ctx context.Context, orderID int, status string) error {
	tx, err := c.write.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		"UPDATE orders SET status = $1 WHERE id = $2", status, orderID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "order_status_changed", orderID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appendEvent(ctx context.Context, tx pgx.Tx, kind string, orderID int) error {
	payload, _ := json.Marshal(map[string]any{"type": kind, "order_id": orderID})
	_, err := tx.Exec(ctx,
		"INSERT INTO events (aggregate, payload) VALUES ('order', $1)", payload)
	return err
}

// --- PROJECTION side ----------------------------------------------------------

// Project consumes unprocessed outbox events and rebuilds the affected read-model
// rows. It reads the normalized write store (a JOIN + aggregate) and writes the
// flat denormalized row into the read store. This is the bridge that makes the
// two models eventually consistent.
func (c *CQRS) Project(ctx context.Context) (int, error) {
	rows, err := c.write.Query(ctx,
		"SELECT id, payload FROM events WHERE NOT processed ORDER BY id")
	if err != nil {
		return 0, err
	}
	type ev struct {
		id      int64
		orderID int
	}
	var batch []ev
	for rows.Next() {
		var id int64
		var payload map[string]any
		if err := rows.Scan(&id, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, ev{id: id, orderID: int(payload["order_id"].(float64))})
	}
	rows.Close()

	processed := 0
	for _, e := range batch {
		if err := c.projectOrder(ctx, e.orderID); err != nil {
			return processed, err
		}
		if _, err := c.write.Exec(ctx,
			"UPDATE events SET processed = TRUE WHERE id = $1", e.id); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// projectOrder builds one denormalized summary row from the normalized write store
// and upserts it into the read store.
func (c *CQRS) projectOrder(ctx context.Context, orderID int) error {
	var name, email, status string
	var itemCount, total int
	var placedAt time.Time
	err := c.write.QueryRow(ctx, `
		SELECT cu.name, cu.email, o.status, o.created_at,
		       COALESCE(COUNT(oi.id), 0),
		       COALESCE(SUM(oi.qty * oi.price_cents), 0)
		FROM orders o
		JOIN customers cu ON cu.id = o.customer_id
		LEFT JOIN order_items oi ON oi.order_id = o.id
		WHERE o.id = $1
		GROUP BY cu.name, cu.email, o.status, o.created_at`,
		orderID).Scan(&name, &email, &status, &placedAt, &itemCount, &total)
	if err != nil {
		return err
	}
	_, err = c.read.Exec(ctx, `
		INSERT INTO order_summary
		    (order_id, customer_name, customer_email, item_count, total_cents, status, placed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (order_id) DO UPDATE SET
		    item_count = EXCLUDED.item_count,
		    total_cents = EXCLUDED.total_cents,
		    status = EXCLUDED.status`,
		orderID, name, email, itemCount, total, status, placedAt)
	return err
}

// --- QUERY side ---------------------------------------------------------------

type Summary struct {
	OrderID      int
	CustomerName string
	ItemCount    int
	TotalCents   int
	Status       string
}

// GetOrderSummary is a query: a single-row lookup against the denormalized read
// store. No JOINs, no aggregation at read time.
func (c *CQRS) GetOrderSummary(ctx context.Context, orderID int) (*Summary, error) {
	var s Summary
	err := c.read.QueryRow(ctx, `
		SELECT order_id, customer_name, item_count, total_cents, status
		FROM order_summary WHERE order_id = $1`, orderID).
		Scan(&s.OrderID, &s.CustomerName, &s.ItemCount, &s.TotalCents, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

func main() {
	ctx := context.Background()

	write, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5460/write_db?sslmode=disable")
	if err != nil {
		log.Fatalf("write store: %v", err)
	}
	defer write.Close()
	read, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5461/read_db?sslmode=disable")
	if err != nil {
		log.Fatalf("read store: %v", err)
	}
	defer read.Close()
	app := &CQRS{write: write, read: read}
	fmt.Println("✅ Connected: write store (5460) + read store (5461)")

	banner("🔀 CQRS SIMULATION")

	section("✍️  COMMAND — place an order (write store + outbox)")
	orderID, err := app.PlaceOrder(ctx, 1, []Item{
		{Product: "Keyboard", Qty: 1, PriceCents: 7999},
		{Product: "Mouse", Qty: 2, PriceCents: 2599},
	})
	if err != nil {
		log.Fatalf("place order: %v", err)
	}
	fmt.Printf("   Order #%d written to normalized store; event queued in outbox\n", orderID)

	section("🔎 QUERY before projection — eventual consistency in action")
	if s, _ := app.GetOrderSummary(ctx, orderID); s == nil {
		fmt.Printf("   Read model has NO row for order #%d yet (projector hasn't run) ⏳\n", orderID)
	}

	section("⚙️  PROJECTOR — drain outbox, rebuild read model")
	n, err := app.Project(ctx)
	if err != nil {
		log.Fatalf("project: %v", err)
	}
	fmt.Printf("   Projected %d event(s) into the read store\n", n)

	section("🔎 QUERY after projection — single-row, pre-joined read")
	printSummary(app, ctx, orderID)

	section("✍️  COMMAND — mark order shipped, then re-project")
	if err := app.UpdateStatus(ctx, orderID, "shipped"); err != nil {
		log.Fatalf("update status: %v", err)
	}
	fmt.Println("   status → 'shipped' (write store); read model still shows old status:")
	printSummary(app, ctx, orderID)
	app.Project(ctx)
	fmt.Println("   after projection:")
	printSummary(app, ctx, orderID)

	section("📊 SUMMARY")
	fmt.Println("   Writes: normalized, transactional, outbox-backed.")
	fmt.Println("   Reads:  denormalized single-row lookups, eventually consistent.")
}

func printSummary(app *CQRS, ctx context.Context, orderID int) {
	s, err := app.GetOrderSummary(ctx, orderID)
	if err != nil {
		log.Printf("query: %v", err)
		return
	}
	if s == nil {
		fmt.Printf("   (no read-model row for order #%d)\n", orderID)
		return
	}
	fmt.Printf("   order #%d | %s | %d items | $%.2f | %s\n",
		s.OrderID, s.CustomerName, s.ItemCount, float64(s.TotalCents)/100, s.Status)
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}
