package order

import "net/http"

// Routes enumerates the slice surface. Slice 5 adds transition CALLERS
// (accept/preparing/ready handlers reusing Transition) as new lines here.
func (h *Handlers) Routes() *http.ServeMux {
	r := http.NewServeMux()
	r.HandleFunc("POST /orders", h.Create)
	r.HandleFunc("POST /orders/", h.Create)
	r.HandleFunc("GET /orders/{id}", h.Get)
	r.HandleFunc("POST /orders/{id}/cancel", h.Cancel)
	return r
}
