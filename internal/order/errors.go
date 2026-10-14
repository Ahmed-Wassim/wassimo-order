package order

import "errors"

var (
	ErrStoreDown     = errors.New("store unavailable")
	ErrEmptyCart     = errors.New("empty cart")
	ErrPriceMoved    = errors.New("price changed")
	ErrBranchChanged = errors.New("branch changed")
	ErrDeadItem      = errors.New("unknown item")
	ErrNoOrder       = errors.New("order not found")
	ErrBadMove       = errors.New("illegal transition")
	ErrKeyConflict   = errors.New("idempotency conflict")
)
