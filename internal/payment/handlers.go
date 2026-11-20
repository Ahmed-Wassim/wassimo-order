package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/ahmed-wassim/wassimo-order/internal/order"
)

type Orders interface {
	Get(ctx context.Context, userID, orderID int64) (*order.Order, error)
	SetPaymentID(ctx context.Context, userID, orderID int64, paymentID string) error
	ReplacePaymentID(ctx context.Context, userID, orderID int64, oldPaymentID, newPaymentID string) error
}

type Handlers struct {
	client *Client
	orders Orders
}

func NewHandlers(client *Client, orders Orders) *Handlers {
	return &Handlers{client: client, orders: orders}
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

func (h *Handlers) fail(w http.ResponseWriter, err error) {
	var se *StatusError
	switch {
	case errors.Is(err, order.ErrNoOrder):
		respond(w, 404, map[string]string{"error": "order not found"})
	case errors.Is(err, order.ErrStoreDown), errors.Is(err, ErrUnavailable):
		respond(w, 503, map[string]string{"error": "dependency unavailable"})
	case errors.Is(err, order.ErrPaymentPointerConflict):
		respond(w, 409, map[string]string{"error": "payment pointer conflict"})
	case errors.As(err, &se):
		if se.Status == 401 || se.Status == 403 {
			log.Printf("payment: upstream auth %d", se.Status)
			respond(w, 503, map[string]string{"error": "dependency unavailable"})
			return
		}
		msg := se.Code
		if msg == "" {
			msg = "payment request failed"
		}
		respond(w, se.Status, map[string]string{"error": msg})
	default:
		respond(w, 500, map[string]string{"error": "internal"})
	}
}

func payableAmountCurrency(o *order.Order) (int64, string) {
	if o.ChargeAmountCents != nil && o.ChargeCurrency != nil && *o.ChargeCurrency != "" {
		return *o.ChargeAmountCents, *o.ChargeCurrency
	}
	return o.Total, o.Currency
}

func (h *Handlers) Options(w http.ResponseWriter, r *http.Request) {
	if _, err := ownerID(r); err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	opts, err := h.client.Options(r.Context(), r.URL.Query().Get("currency"))
	if err != nil {
		h.fail(w, err)
		return
	}
	respond(w, 200, opts)
}

type payRequest struct {
	ProviderAccountID string           `json:"provider_account_id"`
	Method            string           `json:"method"`
	CustomerDetails   *CustomerDetails `json:"customer_details"`
}

func (h *Handlers) Pay(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}

	var in payRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ProviderAccountID == "" || in.Method == "" {
		respond(w, 400, map[string]string{"error": "provider_account_id and method required"})
		return
	}
	if in.CustomerDetails == nil || in.CustomerDetails.Name == "" || in.CustomerDetails.Email == "" {
		respond(w, 400, map[string]string{"error": "customer_details.name and customer_details.email required"})
		return
	}

	o, err := h.orders.Get(r.Context(), owner, orderID)
	if err != nil {
		h.fail(w, err)
		return
	}
	retryFrom := ""
	if o.PaymentID != nil && *o.PaymentID != "" {
		pay, err := h.client.GetPayment(r.Context(), *o.PaymentID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if (pay.Status == "failed" || pay.Status == "expired") && o.Status == "pending" {
			retryFrom = *o.PaymentID
		} else {
			respond(w, 200, map[string]any{
				"order_id":       orderID,
				"payment_id":     *o.PaymentID,
				"redirect_url":   nil,
				"payment_status": pay.Status,
			})
			return
		}
	}
	if o.Status != "pending" {
		respond(w, 409, map[string]string{"error": "order cannot be paid"})
		return
	}

	amount, currency := payableAmountCurrency(o)
	idemKey := fmt.Sprintf("order-pay-%d", orderID)
	reference := fmt.Sprintf("order:%d", orderID)
	if retryFrom != "" {
		idemKey = fmt.Sprintf("order-pay-%d-retry-%s", orderID, retryFrom)
		reference = fmt.Sprintf("order:%d:retry:%s", orderID, retryFrom)
	}
	created, err := h.client.CreatePayment(r.Context(), idemKey, CreatePaymentRequest{
		Reference:         reference,
		AmountMinor:       amount,
		Currency:          currency,
		ProviderAccountID: in.ProviderAccountID,
		Method:            in.Method,
		CustomerDetails:   *in.CustomerDetails,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if retryFrom != "" {
		if err := h.orders.ReplacePaymentID(r.Context(), owner, orderID, retryFrom, created.ID); err != nil {
			h.fail(w, err)
			return
		}
	} else {
		if err := h.orders.SetPaymentID(r.Context(), owner, orderID, created.ID); err != nil {
			h.fail(w, err)
			return
		}
	}
	charge, err := h.client.Charge(r.Context(), created.ID)
	if err != nil {
		respond(w, 502, map[string]any{"order_id": orderID, "payment_id": created.ID, "error": "charge failed"})
		return
	}
	respond(w, 201, map[string]any{
		"order_id":     orderID,
		"payment_id":   created.ID,
		"redirect_url": charge.RedirectURL,
	})
}

func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	o, err := h.orders.Get(r.Context(), owner, orderID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if o.PaymentID == nil || *o.PaymentID == "" {
		respond(w, 404, map[string]string{"error": "payment not found"})
		return
	}
	pay, err := h.client.GetPayment(r.Context(), *o.PaymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"order_id": orderID, "payment": pay})
}

func (h *Handlers) Resolve(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	o, err := h.orders.Get(r.Context(), owner, orderID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if o.PaymentID == nil || *o.PaymentID == "" {
		respond(w, 404, map[string]string{"error": "payment not found"})
		return
	}
	pay, err := h.client.Resolve(r.Context(), *o.PaymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"order_id": orderID, "payment": pay})
}

func (h *Handlers) Refund(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	var in struct {
		AmountMinor int64  `json:"amount_minor"`
		Reason      string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AmountMinor <= 0 {
		respond(w, 400, map[string]string{"error": "amount_minor required"})
		return
	}
	o, err := h.orders.Get(r.Context(), owner, orderID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if o.PaymentID == nil || *o.PaymentID == "" {
		respond(w, 404, map[string]string{"error": "payment not found"})
		return
	}
	refund, err := h.client.RequestRefund(r.Context(), *o.PaymentID, in.AmountMinor, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	respond(w, 201, map[string]any{"order_id": orderID, "refund": refund})
}

func (h *Handlers) ResolveRefund(w http.ResponseWriter, r *http.Request) {
	owner, err := ownerID(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthenticated"})
		return
	}
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		respond(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	refundID := r.PathValue("rid")
	if refundID == "" {
		respond(w, 400, map[string]string{"error": "invalid refund id"})
		return
	}
	o, err := h.orders.Get(r.Context(), owner, orderID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if o.PaymentID == nil || *o.PaymentID == "" {
		respond(w, 404, map[string]string{"error": "payment not found"})
		return
	}
	refund, err := h.client.ResolveRefund(r.Context(), *o.PaymentID, refundID)
	if err != nil {
		h.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"order_id": orderID, "refund": refund})
}
