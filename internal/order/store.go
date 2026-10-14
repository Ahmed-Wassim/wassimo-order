package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmed-wassim/wassimo-order/internal/cart"
	"github.com/ahmed-wassim/wassimo-order/internal/catalog"
)

// Store owns orders. Upstream reads (cart, catalog) arrive as validated
// inputs; only the INSERTs run inside the transaction (02 clients) —
// locks are held for milliseconds, never for network calls.
type Store struct {
	pool    *pgxpool.Pool
	carts   CartReader
	catalog CatalogLookup
}

// CartReader + CatalogLookup are narrow interfaces (not *Client structs) so
// tests inject stubs without HTTP. Same seam rule as cart's Catalog interface.
type CartReader interface {
	GetCart(ctx context.Context, userID int64) (*cart.Cart, error)
}

type CatalogLookup interface {
	Lookup(ctx context.Context, ids []int64) (map[int64]catalog.Item, error)
}

func NewStore(pool *pgxpool.Pool, carts CartReader, catalog CatalogLookup) *Store {
	return &Store{pool: pool, carts: carts, catalog: catalog}
}

// Allowed maps current → permitted next states. Cancel is the only writer
// in this slice; accept/prepare/ready arrive with Slice 5 as new rows,
// never a new mechanism.
var Allowed = map[string][]string{
	"pending": {"cancelled"},
}

func allowed(from, next string) bool {
	for _, n := range Allowed[from] {
		if n == next {
			return true
		}
	}
	return false
}

// Create validates, then inserts order + lines atomically.
func (s *Store) Create(ctx context.Context, userID int64, key string) (*Order, error) {
	cart, err := s.carts.GetCart(ctx, userID)
	if err != nil {
		return nil, ErrStoreDown
	}
	if cart == nil || len(cart.Items) == 0 {
		return nil, ErrEmptyCart
	}
	ids := make([]int64, 0, len(cart.Items))
	for _, l := range cart.Items {
		ids = append(ids, l.CatalogItemID)
	}
	truth, err := s.catalog.Lookup(ctx, ids)
	if err != nil {
		return nil, ErrStoreDown
	}
	for _, l := range cart.Items {
		t, ok := truth[l.CatalogItemID]
		if !ok || !t.IsAvailable {
			return nil, ErrDeadItem
		}
		if t.PriceCents != l.PriceCents || t.Currency != l.Currency || t.Name != l.Name {
			return nil, ErrPriceMoved
		}
		if t.BranchID != cart.BranchID {
			return nil, ErrBranchChanged
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, ErrStoreDown
	}
	defer tx.Rollback(ctx)
	var orderID int64
	var total int64
	for _, l := range cart.Items {
		total += int64(l.PriceCents) * int64(l.Qty)
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, branch_id, status, total_cents, currency, idempotency_key)
		 VALUES ($1,$2,'pending',$3,$4,$5) RETURNING id`,
		userID, cart.BranchID, total, cart.Items[0].Currency, key,
	).Scan(&orderID)
	if err != nil {
		if isDuplicateKey(err) {
			return s.replayOrConflict(ctx, userID, key, cart)
		}
		return nil, fmt.Errorf("insert order: %w", err)
	}
	for _, l := range cart.Items {
		t := truth[l.CatalogItemID]
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_items (order_id, catalog_item_id, name, price_cents, currency, qty)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			orderID, l.CatalogItemID, t.Name, t.PriceCents, t.Currency, l.Qty,
		); err != nil {
			return nil, fmt.Errorf("insert line: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrStoreDown
	}
	return s.Get(ctx, userID, orderID)
}

// isDuplicateKey detects the UNIQUE (user_id, idempotency_key) violation.
func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// replayOrConflict runs on duplicate key: same cart → original (replay,
// 201 same body); changed cart → 409 (not a new order, not the stale one).
func (s *Store) replayOrConflict(ctx context.Context, userID int64, key string, cart *cart.Cart) (*Order, error) {
	original, err := s.getByKey(ctx, userID, key)
	if err != nil {
		return nil, err
	}
	if sameCart(original, cart) {
		return original, nil
	}
	return nil, ErrKeyConflict
}

// Transition validates and applies one move atomically (read status, check
// map, update — concurrent cancels collapse on the row, not in Go).
func (s *Store) Transition(ctx context.Context, orderID, userID int64, next string) (*Order, error) {
	var current string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM orders WHERE id=$1 AND user_id=$2`, orderID, userID,
	).Scan(&current)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoOrder
		}
		return nil, fmt.Errorf("read status: %w", err)
	}
	if !allowed(current, next) {
		return nil, ErrBadMove
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE orders SET status=$1, updated_at=NOW() WHERE id=$2`, next, orderID,
	); err != nil {
		return nil, fmt.Errorf("move order: %w", err)
	}
	return s.Get(ctx, userID, orderID)
}

// Get reads one order with its lines (owner-scoped, same rule as Transition).
func (s *Store) Get(ctx context.Context, userID, orderID int64) (*Order, error) {
	var o Order
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, branch_id, status, total_cents, currency
		 FROM orders WHERE id=$1 AND user_id=$2`, orderID, userID,
	).Scan(&o.ID, &o.UserID, &o.BranchID, &o.Status, &o.Total, &o.Currency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoOrder
		}
		return nil, fmt.Errorf("get order: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT catalog_item_id, name, price_cents, currency, qty
		 FROM order_items WHERE order_id=$1 ORDER BY id`, orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("list lines: %w", err)
	}
	defer rows.Close()
	o.Items = []OrderItem{}
	for rows.Next() {
		var l OrderItem
		if err := rows.Scan(&l.CatalogItemID, &l.Name, &l.PriceCents, &l.Currency, &l.Qty); err != nil {
			return nil, fmt.Errorf("scan line: %w", err)
		}
		o.Items = append(o.Items, l)
	}
	return &o, rows.Err()
}

// getByKey loads the order for a (user, key) pair. Used ONLY on the duplicate
// path — the hot path never pays for it.
func (s *Store) getByKey(ctx context.Context, userID int64, key string) (*Order, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM orders WHERE user_id=$1 AND idempotency_key=$2`, userID, key,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("get by key: %w", err)
	}
	return s.Get(ctx, userID, id)
}

// sameCart decides replay vs conflict: identical lines ⇒ same order.
func sameCart(o *Order, c *cart.Cart) bool {
	if o.BranchID != c.BranchID || len(o.Items) != len(c.Items) {
		return false
	}
	byID := make(map[int64]OrderItem, len(o.Items))
	for _, l := range o.Items {
		byID[l.CatalogItemID] = l
	}
	for _, l := range c.Items {
		ol, ok := byID[l.CatalogItemID]
		if !ok || ol.Qty != l.Qty {
			return false
		}
	}
	return true
}
