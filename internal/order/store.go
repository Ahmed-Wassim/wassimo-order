package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmed-wassim/wassimo-order/internal/cart"
	"github.com/ahmed-wassim/wassimo-order/internal/catalog"
	"github.com/ahmed-wassim/wassimo-order/internal/currency"
	"github.com/ahmed-wassim/wassimo-order/internal/events"
)

// Store — rates is the step 02+03 client (ordered sources); allowed is
// CHARGE_CURRENCIES, the fast door-check list.
type Store struct {
	pool    *pgxpool.Pool
	carts   CartReader
	catalog CatalogLookup

	rates   *currency.Client
	base    string
	allowed map[string]bool
}

type CartReader interface {
	GetCart(ctx context.Context, userID int64) (*cart.Cart, error)
}

type CatalogLookup interface {
	Lookup(ctx context.Context, ids []int64) (map[int64]catalog.Item, error)
}

func NewStore(pool *pgxpool.Pool, carts CartReader, catalog CatalogLookup,
	rateClient *currency.Client, base string, allowed []string) *Store {
	m := make(map[string]bool, len(allowed))
	for _, c := range allowed {
		m[c] = true
	}
	return &Store{pool: pool, carts: carts, catalog: catalog,
		rates: rateClient, base: base, allowed: m}
}

var Allowed = map[string][]string{
	"pending": {"cancelled"},
}

func allowed(from, next string) bool {
	return slices.Contains(Allowed[from], next)
}

func normalizeCharge(in string, base string, allowed map[string]bool) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(in))
	if c == "" {
		return base, nil
	}
	if !allowed[c] {
		return "", ErrBadCurrency
	}
	return c, nil
}

func (s *Store) Create(ctx context.Context, userID int64, key, chargeIn string) (*Order, error) {
	charge, err := normalizeCharge(chargeIn, s.base, s.allowed)
	if err != nil {
		return nil, err
	}

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
	base := cart.Items[0].Currency
	if base != s.base {
		return nil, ErrRateUnavailable
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

	// All five snapshot values are decided BEFORE the insert; the row is
	// born complete or not at all.
	var chargeCurrency any = nil
	var chargeAmountCents any = nil
	var rateUsed any = nil
	var rateSource any = nil
	var fetchedAt any = nil
	if charge != base {
		rate, err := s.rates.Latest(ctx, charge)
		if err != nil {
			return nil, ErrRateUnavailable
		}
		cents, err := currency.Convert(total, rate)
		if err != nil {
			return nil, ErrRateUnavailable
		}
		if cents <= 0 {
			return nil, ErrRateUnavailable
		}
		chargeCurrency = charge
		chargeAmountCents = cents
		rateUsed = rate.Value
		rateSource = rate.Source
		fetchedAt = rate.FetchedAt
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, branch_id, status, total_cents, currency,
		                     idempotency_key, charge_currency, charge_amount_cents,
		                     exchange_rate_used, exchange_rate_source, exchange_rate_fetched_at)
		 VALUES ($1,$2,'pending',$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		userID, cart.BranchID, total, base, key,
		chargeCurrency, chargeAmountCents, rateUsed, rateSource, fetchedAt,
	).Scan(&orderID)
	if err != nil {
		if isDuplicateKey(err) {
			return s.replayOrConflict(ctx, userID, key, cart, charge)
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

func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) replayOrConflict(ctx context.Context, userID int64, key string, cart_ *cart.Cart, charge string) (*Order, error) {
	original, err := s.getByKey(ctx, userID, key)
	if err != nil {
		return nil, err
	}
	if sameCart(original, cart_) && original.chargedCurrency() == charge {
		return original, nil
	}
	return nil, ErrKeyConflict
}

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

func scanSnapshot(o *Order, chargeCurrency sql.NullString, cents sql.NullInt64,
	rate sql.NullString, source sql.NullString, fetched sql.NullTime) {
	if chargeCurrency.Valid {
		o.ChargeCurrency = &chargeCurrency.String
		o.ChargeAmountCents = &cents.Int64
		o.ExchangeRateUsed = &rate.String
		o.ExchangeRateSource = &source.String
		o.ExchangeRateFetchedAt = &fetched.Time
	}
}

func (s *Store) Get(ctx context.Context, userID, orderID int64) (*Order, error) {
	var o Order
	var chargeCurrency sql.NullString
	var cents sql.NullInt64
	var rate, source sql.NullString
	var fetched, paidAt sql.NullTime
	var paymentID sql.NullString
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, branch_id, status, total_cents, currency,
		        charge_currency, charge_amount_cents, exchange_rate_used, exchange_rate_source, exchange_rate_fetched_at,
		        paid_at, payment_id
		 FROM orders WHERE id=$1 AND user_id=$2`, orderID, userID,
	).Scan(&o.ID, &o.UserID, &o.BranchID, &o.Status, &o.Total, &o.Currency,
		&chargeCurrency, &cents, &rate, &source, &fetched, &paidAt, &paymentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoOrder
		}
		return nil, fmt.Errorf("get order: %w", err)
	}
	scanSnapshot(&o, chargeCurrency, cents, rate, source, fetched)
	if paidAt.Valid {
		o.PaidAt = &paidAt.Time
	}
	if paymentID.Valid {
		o.PaymentID = &paymentID.String
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

func (s *Store) ConsumePaymentEvent(ctx context.Context, env events.Envelope) error {
	switch env.EventType {
	case events.EventPaymentSucceeded:
		return s.consumePaymentSucceeded(ctx, env)
	case events.EventPaymentFailed:
		return s.consumePaymentFailed(ctx, env)
	default:
		return nil
	}
}

func (s *Store) consumePaymentSucceeded(ctx context.Context, env events.Envelope) error {
	orderID, err := parseOrderReference(env.Payload.Reference)
	if err != nil {
		log.Printf("events: event %s dropped: %v", env.EventID, err)
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErrStoreDown
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		env.EventID)
	if err != nil {
		return ErrStoreDown
	}
	if tag.RowsAffected() == 0 {
		return nil
	}

	var total int64
	var currency, status string
	err = tx.QueryRow(ctx,
		`SELECT total_cents, currency, status FROM orders WHERE id=$1`,
		orderID,
	).Scan(&total, &currency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Printf("events: event %s: order %d not found — dropped", env.EventID, orderID)
		return nil
	}
	if err != nil {
		return ErrStoreDown
	}

	if env.Payload.AmountMinor != total || env.Payload.Currency != currency {
		log.Printf("events: MISMATCH event %s: order %d says %d %s, event says %d %s",
			env.EventID, orderID, total, currency, env.Payload.AmountMinor, env.Payload.Currency)
	}

	tag, err = tx.Exec(ctx,
		`UPDATE orders
		 SET status='paid', paid_at=NOW(), payment_id=$1, updated_at=NOW()
		 WHERE id=$2 AND status='pending'`,
		env.Payload.PaymentID, orderID)
	if err != nil {
		return ErrStoreDown
	}
	if tag.RowsAffected() == 0 {
		log.Printf("events: event %s: order %d status '%s' refuses 'paid' — recorded, not applied",
			env.EventID, orderID, status)
	}

	return tx.Commit(ctx)
}

func (s *Store) consumePaymentFailed(ctx context.Context, env events.Envelope) error {
	orderID, err := parseOrderReference(env.Payload.Reference)
	if err != nil {
		log.Printf("events: event %s dropped: %v", env.EventID, err)
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErrStoreDown
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		env.EventID)
	if err != nil {
		return ErrStoreDown
	}
	if tag.RowsAffected() == 0 {
		return nil
	}

	log.Printf("events: payment.failed event %s for order %d reason=%q",
		env.EventID, orderID, env.Payload.ReasonCode)

	return tx.Commit(ctx)
}

func (s *Store) SetPaymentID(ctx context.Context, userID, orderID int64, paymentID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE orders SET payment_id = $1, updated_at = NOW()
		 WHERE id = $2 AND user_id = $3
		   AND (payment_id IS NULL OR payment_id = $1::uuid)`,
		paymentID, orderID, userID)
	if err != nil {
		return ErrStoreDown
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing *string
	err = s.pool.QueryRow(ctx,
		`SELECT payment_id::text FROM orders WHERE id = $1 AND user_id = $2`,
		orderID, userID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoOrder
	}
	if err != nil {
		return ErrStoreDown
	}
	if existing != nil && *existing != paymentID {
		return ErrPaymentPointerConflict
	}
	return nil
}

func (s *Store) ReplacePaymentID(ctx context.Context, userID, orderID int64, oldPaymentID, newPaymentID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE orders SET payment_id = $1, updated_at = NOW()
		 WHERE id = $2 AND user_id = $3 AND payment_id = $4::uuid`,
		newPaymentID, orderID, userID, oldPaymentID)
	if err != nil {
		return ErrStoreDown
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing *string
	err = s.pool.QueryRow(ctx,
		`SELECT payment_id::text FROM orders WHERE id = $1 AND user_id = $2`,
		orderID, userID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoOrder
	}
	if err != nil {
		return ErrStoreDown
	}
	if existing != nil && *existing == newPaymentID {
		return nil
	}
	return ErrPaymentPointerConflict
}

func parseOrderReference(reference string) (int64, error) {
	const prefix = "order:"
	if !strings.HasPrefix(reference, prefix) {
		return 0, fmt.Errorf("unrelated reference %q", reference)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(reference, prefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("malformed reference %q", reference)
	}
	return id, nil
}
