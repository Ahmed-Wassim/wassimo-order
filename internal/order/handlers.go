package order

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Handlers struct {
	store *Store
	pool  *pgxpool.Pool
}

func NewHandlers(store *Store, pool *pgxpool.Pool) *Handlers {
	return &Handlers{store: store, pool: pool}
}

func ownerID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("missing owner")
	}
	return id, nil
}

func respond(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrEmptyCart), errors.Is(err, ErrPriceMoved),
		errors.Is(err, ErrDeadItem), errors.Is(err, ErrBranchChanged):
		respond(w, 422, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrBadCurrency):
		respond(w, 422, map[string]string{"error": "unsupported currency"})
	case errors.Is(err, ErrRateUnavailable):
		respond(w, 503, map[string]string{"error": "rate unavailable"})
	case errors.Is(err, ErrNoOrder):
		respond(w, 404, map[string]string{"error": "order not found"})
	case errors.Is(err, ErrBadMove):
		respond(w, 422, map[string]string{"error": "illegal transition"})
	case errors.Is(err, ErrKeyConflict):
		respond(w, 409, map[string]string{"error": "idempotency conflict"})
	case errors.Is(err, ErrStoreDown):
		respond(w, 503, map[string]string{"error": "dependency unavailable"})
	default:
		respond(w, 500, map[string]string{"error": "internal"})
	}
}

func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	var in struct {
		Key      string `json:"idempotency_key"`
		Currency string `json:"currency"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Key == "" {
		respond(w, 400, map[string]string{"error": "idempotency_key required"})
		return
	}
	order, err := h.store.Create(r.Context(), owner, in.Key, in.Currency)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 201, map[string]any{"order": order})
}

func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	order, err := h.store.Get(r.Context(), owner, id)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"order": order})
}

func (h *Handlers) Cancel(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	order, err := h.store.Transition(r.Context(), id, owner, "cancelled")
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"order": order})
}

func (h *Handlers) Rates(w http.ResponseWriter, r *http.Request) {
	rates, broken := h.store.rates.QuoteAll(r.Context())
	if len(rates) == 0 {
		respond(w, 503, map[string]any{"error": "rate unavailable"})
		return
	}

	type pair struct {
		Quote     string `json:"quote"`
		Pair      string `json:"pair"`
		Value     string `json:"value"`
		Source    string `json:"source"`
		FetchedAt string `json:"fetched_at"`
	}
	out := make([]pair, 0, len(rates))
	for _, rt := range rates {
		out = append(out, pair{
			Quote:     strings.TrimPrefix(rt.Pair, h.store.base+"/"),
			Pair:      rt.Pair,
			Value:     rt.Value,
			Source:    rt.Source,
			FetchedAt: rt.FetchedAt.UTC().Format(time.RFC3339),
		})
	}

	payload := map[string]any{
		"base":  h.store.base,
		"rates": out,
	}
	if len(broken) > 0 {
		payload["broken"] = broken
	}
	respond(w, 200, payload)
}

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]string{"status": "ok"})
}

func (h *Handlers) Ready(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		respond(w, 503, map[string]string{"status": "unavailable"})
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}
