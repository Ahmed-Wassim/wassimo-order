package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("payment service unavailable")

type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("payment %d: %s", e.Status, e.Code)
	}
	return fmt.Sprintf("payment %d", e.Status)
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

type Option struct {
	ProviderAccountID string   `json:"provider_account_id"`
	Gateway           string   `json:"gateway"`
	DisplayName       string   `json:"display_name"`
	Methods           []string `json:"methods"`
	Currencies        []string `json:"currencies"`
}

type OptionsResponse struct {
	Organization string   `json:"organization"`
	GeneratedAt  string   `json:"generated_at"`
	Options      []Option `json:"options"`
}

type CustomerDetails struct {
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Phone       *string `json:"phone,omitempty"`
	CountryCode *string `json:"country_code,omitempty"`
}

type CreatePaymentRequest struct {
	Reference         string          `json:"reference"`
	AmountMinor       int64           `json:"amount_minor"`
	Currency          string          `json:"currency"`
	ProviderAccountID string          `json:"provider_account_id"`
	Method            string          `json:"method"`
	CustomerDetails   CustomerDetails `json:"customer_details"`
}

type PaymentModel struct {
	ID                     string           `json:"id"`
	Reference              string           `json:"reference"`
	AmountMinor            int64            `json:"amount_minor"`
	Currency               string           `json:"currency"`
	Status                 string           `json:"status"`
	StatusDetail           string           `json:"status_detail"`
	CapturedMinor          int64            `json:"captured_minor"`
	RefundedConfirmedMinor int64            `json:"refunded_confirmed_minor"`
	Method                 *string          `json:"method"`
	CustomerDetails        *CustomerDetails `json:"customer_details"`
	CreatedAt              string           `json:"created_at"`
	UpdatedAt              string           `json:"updated_at"`
}

type ChargeResponse struct {
	AttemptID   string  `json:"attempt_id"`
	RedirectURL *string `json:"redirect_url"`
}

type RefundModel struct {
	ID          string  `json:"id"`
	PaymentID   string  `json:"payment_id"`
	AmountMinor int64   `json:"amount_minor"`
	Currency    string  `json:"currency"`
	Status      string  `json:"status"`
	ProviderRef *string `json:"provider_ref"`
	Reason      *string `json:"reason"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func (c *Client) Options(ctx context.Context, currency string) (OptionsResponse, error) {
	path := "/v1/options"
	if currency != "" {
		path += "?currency=" + currency
	}
	var out OptionsResponse
	if err := c.do(ctx, http.MethodGet, path, "", nil, &out); err != nil {
		return OptionsResponse{}, err
	}
	return out, nil
}

func (c *Client) CreatePayment(ctx context.Context, idemKey string, req CreatePaymentRequest) (PaymentModel, error) {
	var out PaymentModel
	if err := c.do(ctx, http.MethodPost, "/v1/payments", idemKey, req, &out); err != nil {
		return PaymentModel{}, err
	}
	return out, nil
}

func (c *Client) Charge(ctx context.Context, paymentID string) (ChargeResponse, error) {
	var out ChargeResponse
	if err := c.do(ctx, http.MethodPost, "/v1/payments/"+paymentID+"/charge", "", nil, &out); err != nil {
		return ChargeResponse{}, err
	}
	return out, nil
}

func (c *Client) GetPayment(ctx context.Context, paymentID string) (PaymentModel, error) {
	var out PaymentModel
	if err := c.do(ctx, http.MethodGet, "/v1/payments/"+paymentID, "", nil, &out); err != nil {
		return PaymentModel{}, err
	}
	return out, nil
}

func (c *Client) Resolve(ctx context.Context, paymentID string) (PaymentModel, error) {
	var out PaymentModel
	if err := c.do(ctx, http.MethodPost, "/v1/payments/"+paymentID+"/resolve", "", nil, &out); err != nil {
		return PaymentModel{}, err
	}
	return out, nil
}

func (c *Client) RequestRefund(ctx context.Context, paymentID string, amountMinor int64, reason string) (RefundModel, error) {
	body := map[string]any{"amount_minor": amountMinor}
	if reason != "" {
		body["reason"] = reason
	}
	var out RefundModel
	if err := c.do(ctx, http.MethodPost, "/v1/payments/"+paymentID+"/refunds", "", body, &out); err != nil {
		return RefundModel{}, err
	}
	return out, nil
}

func (c *Client) ResolveRefund(ctx context.Context, paymentID, refundID string) (RefundModel, error) {
	var out RefundModel
	if err := c.do(ctx, http.MethodPost, "/v1/payments/"+paymentID+"/refunds/"+refundID+"/resolve", "", nil, &out); err != nil {
		return RefundModel{}, err
	}
	return out, nil
}

func (c *Client) do(ctx context.Context, method, path, idemKey string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return err
			}
		}
		return nil
	}
	if res.StatusCode >= 500 {
		return ErrUnavailable
	}

	code := ""
	var errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(data, &errBody) == nil {
		if errBody.Code != "" {
			code = errBody.Code
		} else {
			code = errBody.Error
		}
	}
	return &StatusError{Status: res.StatusCode, Code: code}
}
