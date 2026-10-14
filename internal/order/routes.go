package order

import "net/http"

func (h *Handlers) Routes() *http.ServeMux {
	r := http.NewServeMux()
	r.HandleFunc("POST /orders", h.Create)
	r.HandleFunc("POST /orders/", h.Create)
	r.HandleFunc("GET /orders/{id}", h.Get)
	r.HandleFunc("POST /orders/{id}/cancel", h.Cancel)
	return r
}
