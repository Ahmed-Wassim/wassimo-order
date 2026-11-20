package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
	EventVersion1         = "1"
)

type PaymentPayload struct {
	PaymentID   string `json:"payment_id"`
	Reference   string `json:"reference"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	ReasonCode  string `json:"reason_code,omitempty"`
	Message     string `json:"message,omitempty"`
}

type Envelope struct {
	EventID       string
	EventType     string
	Version       string
	OccurredAt    time.Time
	CorrelationID string
	Producer      string
	Payload       PaymentPayload
}

func DecodeEnvelope(body []byte) (Envelope, error) {
	var raw struct {
		EventID       string          `json:"event_id"`
		EventType     string          `json:"event_type"`
		Version       string          `json:"version"`
		OccurredAt    string          `json:"occurred_at"`
		CorrelationID string          `json:"correlation_id"`
		Producer      string          `json:"producer"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Envelope{}, fmt.Errorf("envelope is not JSON: %w", err)
	}
	if raw.EventID == "" {
		return Envelope{}, errors.New("envelope: missing event_id")
	}
	if raw.EventType != EventPaymentSucceeded && raw.EventType != EventPaymentFailed {
		return Envelope{}, fmt.Errorf("envelope: unsupported event_type %q", raw.EventType)
	}
	if raw.Version != EventVersion1 {
		return Envelope{}, fmt.Errorf("envelope: unsupported version %q", raw.Version)
	}
	occurredAt, err := time.Parse(time.RFC3339, raw.OccurredAt)
	if err != nil {
		return Envelope{}, errors.New("envelope: occurred_at is not RFC3339")
	}

	var payload PaymentPayload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return Envelope{}, errors.New("envelope: invalid payment payload")
	}
	if payload.PaymentID == "" {
		return Envelope{}, errors.New("envelope: payload missing payment_id")
	}
	if payload.Reference == "" {
		return Envelope{}, errors.New("envelope: payload missing reference")
	}
	if raw.EventType == EventPaymentSucceeded && payload.AmountMinor <= 0 {
		return Envelope{}, errors.New("envelope: payload amount_minor not positive")
	}

	return Envelope{
		EventID:       raw.EventID,
		EventType:     raw.EventType,
		Version:       raw.Version,
		OccurredAt:    occurredAt,
		CorrelationID: raw.CorrelationID,
		Producer:      raw.Producer,
		Payload:       payload,
	}, nil
}
