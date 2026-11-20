package order

import "time"

type OrderItem struct {
	CatalogItemID int64  `json:"catalog_item_id"`
	Name          string `json:"name"`
	PriceCents    int64  `json:"price_cents"`
	Currency      string `json:"currency"`
	Qty           int    `json:"qty"`
}

// Order — base fields are always present (the ledger); the charge snapshot
// is all-or-none and JSON-omitted when absent.
type Order struct {
	ID       int64       `json:"id"`
	UserID   int64       `json:"user_id"`
	BranchID int64       `json:"branch_id"`
	Status   string      `json:"status"`
	Items    []OrderItem `json:"items"`
	Total    int64       `json:"total_cents"`
	Currency string      `json:"currency"`

	ChargeCurrency        *string    `json:"charge_currency,omitempty"`
	ChargeAmountCents     *int64     `json:"charge_amount_cents,omitempty"`
	ExchangeRateUsed      *string    `json:"exchange_rate_used,omitempty"`
	ExchangeRateSource    *string    `json:"exchange_rate_source,omitempty"`
	ExchangeRateFetchedAt *time.Time `json:"exchange_rate_fetched_at,omitempty"`

	PaidAt    *time.Time `json:"paid_at,omitempty"`
	PaymentID *string    `json:"payment_id,omitempty"`
}

func (o *Order) chargedCurrency() string {
	if o.ChargeCurrency != nil {
		return *o.ChargeCurrency
	}
	return o.Currency
}

func (o *Order) chargesAmount() int64 {
	if o.ChargeAmountCents != nil {
		return *o.ChargeAmountCents
	}
	return o.Total
}
