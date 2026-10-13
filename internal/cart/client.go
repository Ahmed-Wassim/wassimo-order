package cart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type CartLine struct {
	ID            int64  `json:"id"`
	CatalogItemID int64  `json:"catalog_item_id"`
	Name          string `json:"name"`
	PriceCents    int    `json:"price_cents"`
	Currency      string `json:"currency"`
	Qty           int    `json:"qty"`
}

type Cart struct {
	ID       int64      `json:"id"`
	UserID   int64      `json:"user_id"`
	BranchID int64      `json:"branch_id"`
	Items    []CartLine `json:"items"`
}

type Client struct {
	base   string
	client *http.Client
}

func New(base string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *Client) GetCart(ctx context.Context, userID int64) (*Cart, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/cart", nil)
	if err != nil {
		return nil, fmt.Errorf("cart read build request: %w", err)
	}

	req.Header.Set("X-User-Id", fmt.Sprintf("%d", userID))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cart read call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cart read status %d", resp.StatusCode)
	}

	var envelope struct {
		Cart *Cart `json:"cart"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("cart read decode: %w", err)
	}
	return envelope.Cart, nil
}
