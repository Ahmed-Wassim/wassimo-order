package events

import (
	"context"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeName = "wassimo.events"
	DeadExchange = "wassimo.events.dead"
	QueueName    = "orders.payment-events"
	DLQName      = "orders.payment-events.dead"
)

type Handler func(ctx context.Context, env Envelope) error

func Run(ctx context.Context, url string, handle Handler) {
	for {
		err := runOnce(ctx, url, handle)
		if ctx.Err() != nil {
			return
		}
		log.Printf("events: consumer stopped: %v (reconnecting)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func runOnce(ctx context.Context, url string, handle Handler) error {
	conn, err := amqp.Dial(url)
	if err != nil {
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := ch.ExchangeDeclare(ExchangeName, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(DeadExchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(DLQName, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(DLQName, "#", DeadExchange, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(QueueName, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": DeadExchange,
	}); err != nil {
		return err
	}
	for _, key := range []string{EventPaymentSucceeded, EventPaymentFailed} {
		if err := ch.QueueBind(QueueName, key, ExchangeName, false, nil); err != nil {
			return err
		}
	}
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}

	deliveries, err := ch.Consume(QueueName, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("events: consuming %s", QueueName)

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return amqp.ErrClosed
			}
			env, err := DecodeEnvelope(d.Body)
			if err != nil {
				log.Printf("events: poison message dead-lettered: %v", err)
				_ = d.Nack(false, false)
				continue
			}
			if err := runWithRetry(ctx, handle, env); err != nil {
				log.Printf("events: handler failed (event_id=%s): %v — dead-lettering", env.EventID, err)
				_ = d.Nack(false, false)
				continue
			}
			if err := d.Ack(false); err != nil {
				log.Printf("events: ack failed (event_id=%s): %v", env.EventID, err)
			}
		}
	}
}

func runWithRetry(ctx context.Context, handle Handler, env Envelope) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		if err = handle(ctx, env); err == nil {
			return nil
		}
	}
	return err
}
