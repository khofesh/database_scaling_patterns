package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event sourcing models a bank account. We never store "balance = 120". Instead we
// store the facts that happened — AccountOpened, Deposited, Withdrew — and derive
// the balance by replaying them. The log is the source of truth; everything else
// (current state, snapshots) is a projection of it.

// --- DOMAIN: state + events ---------------------------------------------------

// Account is the *derived* state. It exists only in memory, rebuilt from events.
type Account struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Balance int    `json:"balance_cents"`
	Open    bool   `json:"open"`
	Version int    `json:"version"` // last event version folded into this state
}

// apply folds a single event into the state. This is the heart of event sourcing:
// state(n) = apply(state(n-1), event(n)). It must be a pure function of the event.
func (a *Account) apply(eventType string, payload map[string]any) {
	switch eventType {
	case "AccountOpened":
		a.Owner = payload["owner"].(string)
		a.Open = true
	case "Deposited":
		a.Balance += int(payload["amount"].(float64))
	case "Withdrew":
		a.Balance -= int(payload["amount"].(float64))
	case "AccountClosed":
		a.Open = false
	}
	a.Version++
}

// --- EVENT STORE --------------------------------------------------------------

type Store struct{ db *pgxpool.Pool }

// ErrConcurrency is returned when another writer advanced the aggregate first.
var ErrConcurrency = errors.New("concurrent modification: aggregate version moved")

// load rebuilds an aggregate's current state. It starts from the newest snapshot
// (if any) and replays only the events committed *after* that snapshot. With no
// snapshot it replays from version 0 — the full history.
func (s *Store) load(ctx context.Context, id string) (*Account, int, error) {
	acc := &Account{ID: id}
	fromVersion := 0

	// Try the latest snapshot first — this is the optimization that keeps replay
	// cheap as histories grow into the millions of events.
	var snapState []byte
	var snapVersion int
	err := s.db.QueryRow(ctx,
		`SELECT version, state FROM snapshots
		 WHERE aggregate_id = $1 ORDER BY version DESC LIMIT 1`, id).
		Scan(&snapVersion, &snapState)
	if err == nil {
		if uerr := json.Unmarshal(snapState, acc); uerr != nil {
			return nil, 0, uerr
		}
		fromVersion = snapVersion
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT version, event_type, payload FROM events
		 WHERE aggregate_id = $1 AND version > $2 ORDER BY version`, id, fromVersion)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	replayed := 0
	for rows.Next() {
		var version int
		var eventType string
		var payload map[string]any
		if err := rows.Scan(&version, &eventType, &payload); err != nil {
			return nil, 0, err
		}
		acc.apply(eventType, payload)
		replayed++
	}
	return acc, replayed, rows.Err()
}

// append writes one new event at expectedVersion+1. If another writer already
// committed that version, the UNIQUE(aggregate_id, version) constraint rejects us
// and we surface ErrConcurrency — optimistic concurrency control with no locks.
func (s *Store) append(ctx context.Context, id string, expectedVersion int, eventType string, payload map[string]any) error {
	data, _ := json.Marshal(payload)
	_, err := s.db.Exec(ctx,
		`INSERT INTO events (aggregate_id, version, event_type, payload)
		 VALUES ($1, $2, $3, $4)`,
		id, expectedVersion+1, eventType, data)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrConcurrency
	}
	return err
}

// snapshot folds the full current state and caches it, so future loads can skip
// replaying everything before this version.
func (s *Store) snapshot(ctx context.Context, acc *Account) error {
	data, _ := json.Marshal(acc)
	_, err := s.db.Exec(ctx,
		`INSERT INTO snapshots (aggregate_id, version, state) VALUES ($1, $2, $3)
		 ON CONFLICT (aggregate_id, version) DO NOTHING`,
		acc.ID, acc.Version, data)
	return err
}

// --- COMMANDS (load → validate → append) --------------------------------------

func (s *Store) OpenAccount(ctx context.Context, id, owner string) error {
	acc, _, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if acc.Open {
		return fmt.Errorf("account %s already open", id)
	}
	return s.append(ctx, id, acc.Version, "AccountOpened", map[string]any{"owner": owner})
}

func (s *Store) Deposit(ctx context.Context, id string, amount int) error {
	acc, _, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if !acc.Open {
		return fmt.Errorf("account %s is not open", id)
	}
	return s.append(ctx, id, acc.Version, "Deposited", map[string]any{"amount": amount})
}

func (s *Store) Withdraw(ctx context.Context, id string, amount int) error {
	acc, _, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if !acc.Open {
		return fmt.Errorf("account %s is not open", id)
	}
	if acc.Balance < amount { // a business rule enforced against derived state
		return fmt.Errorf("insufficient funds: balance %d < %d", acc.Balance, amount)
	}
	return s.append(ctx, id, acc.Version, "Withdrew", map[string]any{"amount": amount})
}

func main() {
	ctx := context.Background()

	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5490/eventstore?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	fmt.Println("✅ Connected to event store (5490)")

	const acc = "acct-1001"
	// Start clean so the demo is repeatable.
	db.Exec(ctx, "DELETE FROM events WHERE aggregate_id = $1", acc)
	db.Exec(ctx, "DELETE FROM snapshots WHERE aggregate_id = $1", acc)

	banner("📒 EVENT SOURCING SIMULATION")

	section("✍️  COMMANDS — every change is appended as an immutable event")
	must(store.OpenAccount(ctx, acc, "Alice Wong"))
	must(store.Deposit(ctx, acc, 10000)) // $100.00
	must(store.Deposit(ctx, acc, 5000))  // $50.00
	must(store.Withdraw(ctx, acc, 3000)) // $30.00
	fmt.Println("   Appended: AccountOpened, Deposited, Deposited, Withdrew")

	section("🔁 REBUILD — derive current state by replaying the log")
	state, replayed, err := store.load(ctx, acc)
	must(err)
	fmt.Printf("   Replayed %d events → %s | balance $%.2f | version %d\n",
		replayed, state.Owner, float64(state.Balance)/100, state.Version)

	section("🚫 BUSINESS RULE — validated against derived state")
	if err := store.Withdraw(ctx, acc, 999999); err != nil {
		fmt.Printf("   Withdraw $9999.99 rejected: %v\n", err)
	}

	section("📸 SNAPSHOT — cache the fold so future loads skip old events")
	must(store.snapshot(ctx, state))
	fmt.Printf("   Snapshot saved at version %d\n", state.Version)
	must(store.Deposit(ctx, acc, 2500)) // one more event after the snapshot
	state, replayed, err = store.load(ctx, acc)
	must(err)
	fmt.Printf("   Reload replayed only %d event(s) (post-snapshot) → balance $%.2f, version %d\n",
		replayed, float64(state.Balance)/100, state.Version)

	section("⚔️  OPTIMISTIC CONCURRENCY — two writers race for the next version")
	a, _, _ := store.load(ctx, acc)
	b, _, _ := store.load(ctx, acc) // both read the SAME version
	if err := store.append(ctx, acc, a.Version, "Deposited", map[string]any{"amount": 100}); err != nil {
		fmt.Printf("   Writer A failed: %v\n", err)
	} else {
		fmt.Printf("   Writer A committed version %d ✅\n", a.Version+1)
	}
	if err := store.append(ctx, acc, b.Version, "Deposited", map[string]any{"amount": 200}); err != nil {
		fmt.Printf("   Writer B rejected: %v ✅ (must reload & retry)\n", err)
	}

	section("🧾 AUDIT LOG — the full immutable history is always available")
	printHistory(ctx, db, acc)

	section("📊 SUMMARY")
	fmt.Println("   Truth = append-only event log; state is derived by replay.")
	fmt.Println("   Snapshots make replay cheap; concurrency is optimistic via version.")
}

func printHistory(ctx context.Context, db *pgxpool.Pool, id string) {
	rows, err := db.Query(ctx,
		`SELECT version, event_type, payload FROM events
		 WHERE aggregate_id = $1 ORDER BY version`, id)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var t string
		var p map[string]any
		rows.Scan(&v, &t, &p)
		fmt.Printf("   v%-2d  %-14s %v\n", v, t, p)
	}
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
