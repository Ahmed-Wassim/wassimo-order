package order

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Handlers holds the store. Constructed once in main, methods are the routes.
type Handlers struct {
	store *Store
	pool  *pgxpool.Pool
}

func NewHandlers(store *Store, pool *pgxpool.Pool) *Handlers {
	return &Handlers{store: store, pool: pool}
}

// ownerID reads the gateway-verified owner (same rule as cart: never a body
// field — a client-supplied user_id would let anyone order as anyone).
func ownerID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("missing owner")
	}
	return id, nil
}

// respond is the ONLY success writer; fail maps store errors to contract
// codes (unmapped errors are 500 with a fixed message — details go to logs).
func respond(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrEmptyCart), errors.Is(err, ErrPriceMoved), errors.Is(err, ErrDeadItem), errors.Is(err, ErrBranchChanged):
		respond(w, 422, map[string]string{"error": err.Error()})
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

// Create checks out the cart. Always returns the full order — mobile clears
// its own cart after 201 (documented split, 00-contract).
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	var in struct {
		Key string `json:"idempotency_key"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Key == "" {
		respond(w, 400, map[string]string{"error": "idempotency_key required"})
		return
	}
	order, err := h.store.Create(r.Context(), owner, in.Key)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 201, map[string]any{"order": order})
}

// Get reads one order (owner-scoped).
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

// Cancel moves pending → cancelled via the transition table (nothing else).
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

// Health never touches the database; Ready pings order_db only.
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
