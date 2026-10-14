package order

import "errors"

var (
	ErrStoreDown     = errors.New("store unavailable")    // → 503 (tx begin/commit fails)
	ErrEmptyCart     = errors.New("empty cart")           // → 422 (nothing to buy)
	ErrPriceMoved    = errors.New("price changed")        // → 422 (snapshot ≠ truth)
	ErrBranchChanged = errors.New("branch changed")       // → 422 (cart branch moved)
	ErrDeadItem      = errors.New("unknown item")         // → 422 (absent/unavailable now)
	ErrNoOrder       = errors.New("order not found")      // → 404 (not yours: unenumerable)
	ErrBadMove       = errors.New("illegal transition")   // → 422 (machine says no)
	ErrKeyConflict   = errors.New("idempotency conflict") // → 409 (same key, different cart)
)
