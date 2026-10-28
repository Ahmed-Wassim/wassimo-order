package currency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sources are ORDERED (ADR-023): the client tries them in order and uses
// the first healthy answer — rescue on failure, never election.

// Rate — one conversion factor; Pair is always BASE/QUOTE.
type Rate struct {
	Pair      string    `json:"pair"`
	Value     string    `json:"value"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
}

// ErrUnavailable maps to 503 rate_unavailable in checkout.
var ErrUnavailable = errors.New("exchange rate unavailable")

// Source — one provider in the ordered list. Name is both the adapter
// key and the string recorded on orders.exchange_rate_source.
type Source struct {
	Name string // "frankfurter" | "er-api"
	URL  string
}

type Client struct {
	base    string          // ledger currency ("EGP")
	quotes  map[string]bool // quote codes this instance serves
	sources []Source        // ordered; first healthy answer wins
	http    *http.Client
}

func New(base string, quotes []string, sources []Source, timeout time.Duration) *Client {
	c := &Client{
		base:    strings.ToUpper(strings.TrimSpace(base)),
		quotes:  map[string]bool{},
		sources: sources,
		http:    &http.Client{Timeout: timeout},
	}
	for _, q := range quotes {
		c.quotes[strings.ToUpper(strings.TrimSpace(q))] = true
	}
	return c
}

// KnownSource — adapters this package can talk to; main.go boot-refuses
// unknown names so a typo'd config never becomes a per-request outage.
func KnownSource(name string) bool {
	return name == "frankfurter" || name == "er-api"
}

func (c *Client) Latest(ctx context.Context, quote string) (Rate, error) {
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if quote == c.base {
		return Rate{}, fmt.Errorf("the base currency needs no rate: %w", ErrUnavailable)
	}
	if !c.quotes[quote] {
		return Rate{}, fmt.Errorf("unsupported quote %q: %w", quote, ErrUnavailable)
	}
	for _, s := range c.sources {
		if r, err := c.fetchSource(ctx, s, quote); err == nil {
			return r, nil
		}
	}
	return Rate{}, ErrUnavailable
}

// QuoteAll — ordered sources per pair, parallel across pairs; a pair is
// "broken" only when every source failed.
func (c *Client) QuoteAll(ctx context.Context) ([]Rate, []string) {
	quotes := make([]string, 0, len(c.quotes))
	for q := range c.quotes {
		quotes = append(quotes, q)
	}
	sort.Strings(quotes)

	rates := make([]Rate, len(quotes))
	broken := make([]string, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, q := range quotes {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			for _, s := range c.sources {
				if r, err := c.fetchSource(ctx, s, q); err == nil {
					rates[i] = r
					return
				}
			}
			mu.Lock()
			broken = append(broken, q)
			mu.Unlock()
		}(i, q)
	}
	wg.Wait()

	good := make([]Rate, 0, len(quotes))
	for _, r := range rates {
		if r.Value != "" {
			good = append(good, r)
		}
	}
	return good, broken
}

// fetchSource — GET the pair from source s (the only provider-specific code):
//   - frankfurter v2: {URL}/v2/rate/EGP/USD → {"rate":0.017835}
//   - er-api:         {URL}/v6/latest/EGP   → {"result":"success","rates":{...}}
func (c *Client) fetchSource(ctx context.Context, s Source, quote string) (Rate, error) {
	var u string
	switch s.Name {
	case "frankfurter":
		u = fmt.Sprintf("%s/v2/rate/%s/%s", s.URL, c.base, quote)
	case "er-api":
		u = fmt.Sprintf("%s/v6/latest/%s", s.URL, c.base)
	default:
		return Rate{}, fmt.Errorf("unknown rate source adapter %q: %w", s.Name, ErrUnavailable)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: rate source %s unreachable: %w", ErrUnavailable, s.Name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Rate{}, fmt.Errorf("%w: rate source %s responded %d", ErrUnavailable, s.Name, res.StatusCode)
	}

	var payload struct {
		Rate  json.Number    `json:"rate"`  // frankfurter v2
		Rates map[string]any `json:"rates"` // er-api
	}
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return Rate{}, fmt.Errorf("%w: source %s bad body: %w", ErrUnavailable, s.Name, err)
	}

	var num json.Number
	switch s.Name {
	case "frankfurter":
		num = payload.Rate
	case "er-api":
		raw, ok := payload.Rates[quote]
		n, isNumber := raw.(json.Number)
		if !ok || !isNumber {
			num = json.Number("")
		} else {
			num = n
		}
	}
	if num.String() == "" {
		return Rate{}, fmt.Errorf("%w: no rate for %q from source %s", ErrUnavailable, quote, s.Name)
	}
	rat, ok := new(big.Rat).SetString(num.String())
	if !ok || rat.Sign() <= 0 {
		return Rate{}, fmt.Errorf("%w: rate %q from %s not a positive decimal", ErrUnavailable, num.String(), s.Name)
	}
	return Rate{
		Pair:      c.base + "/" + quote,
		Value:     num.String(),
		Source:    s.Name,
		FetchedAt: time.Now().UTC(),
	}, nil
}

// Convert — total cents × exact rate text → integer cents, half-up once.
func Convert(totalCents int64, r Rate) (int64, error) {
	if totalCents <= 0 {
		return 0, fmt.Errorf("non-positive total for conversion: %w", ErrUnavailable)
	}
	rat, ok := new(big.Rat).SetString(r.Value)
	if !ok || rat.Sign() <= 0 {
		return 0, fmt.Errorf("bad rate %q: %w", r.Value, ErrUnavailable)
	}

	exact := new(big.Rat).Mul(rat, new(big.Rat).SetInt64(totalCents))
	q, rem := new(big.Int).QuoRem(exact.Num(), exact.Denom(), new(big.Int))
	twice := new(big.Int).Abs(new(big.Int).Lsh(rem, 1))
	if twice.Cmp(exact.Denom()) >= 0 {
		if exact.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("converted amount overflows int64: %w", ErrUnavailable)
	}
	return q.Int64(), nil
}
