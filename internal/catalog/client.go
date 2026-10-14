package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Item struct {
	ID          int64  `json:"id"`
	BranchID    int64  `json:"branch_id"`
	Name        string `json:"name"`
	PriceCents  int    `json:"price_cents"`
	Currency    string `json:"currency"`
	IsAvailable bool   `json:"is_available"`
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

func (c *Client) Lookup(ctx context.Context, ids []int64) (map[int64]Item, error) {
	out := map[int64]Item{}
	if len(ids) == 0 {
		return out, nil
	}

	texts := make([]string, 0, len(ids))
	for _, id := range ids {
		texts = append(texts, fmt.Sprintf("%d", id))
	}
	endpoint := c.base + "/internal/items?ids=" + url.QueryEscape(strings.Join(texts, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog lookup build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog lookup call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog lookup status %d", resp.StatusCode)
	}
	var items []Item
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("catalog lookup decode: %w", err)
	}
	for _, item := range items {
		out[item.ID] = item
	}
	return out, nil
}
