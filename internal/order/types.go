package order

type OrderItem struct {
	CatalogItemID int64  `json:"catalog_item_id"`
	Name          string `json:"name"`
	PriceCents    int    `json:"price_cents"`
	Currency      string `json:"currency"`
	Qty           int    `json:"qty"`
}

type Order struct {
	ID       int64       `json:"id"`
	UserID   int64       `json:"user_id"`
	BranchID int64       `json:"branch_id"`
	Status   string      `json:"status"`
	Items    []OrderItem `json:"items"`
	Total    int64       `json:"total_cents"`
	Currency string      `json:"currency"`
}
