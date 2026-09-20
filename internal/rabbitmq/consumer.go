package rabbitmq

import (
	"context"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Consumer consumes AgentRequest messages from a host's request queue
// (<hostid>.request).
type Consumer struct {
	conn      *Connection
	queueName string
	logger    *slog.Logger
}

// NewConsumer creates a new message consumer for the given request queue.
func NewConsumer(conn *Connection, queueName string, logger *slog.Logger) *Consumer {
	return &Consumer{conn: conn, queueName: queueName, logger: logger}
}

// Run subscribes to the request queue and keeps that subscription alive for
// the lifetime of ctx, re-subscribing with backoff whenever the AMQP channel
// or the whole connection drops. The returned channel is closed only when ctx
// is done, so callers never have to restart the consumer themselves - which is
// what used to leave the agent deaf to new tasks after a reconnect hiccup.
func (c *Consumer) Run(ctx context.Context) <-chan amqp.Delivery {
	out := make(chan amqp.Delivery)

	go func() {
		defer close(out)

		attempt := 0
		for {
			if ctx.Err() != nil {
				return
			}

			ch, deliveries, err := c.subscribe()
			if err != nil {
				attempt++
				backoff := calculateBackoff(attempt)
				c.logger.Warn("consumer: cannot subscribe to task queue, retrying",
					"queue", c.queueName, "attempt", attempt, "backoff", backoff.String(), "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
					continue
				}
			}
			attempt = 0

			c.pump(ctx, deliveries, out)
			ch.Close()

			if ctx.Err() != nil {
				return
			}
			c.logger.Warn("consumer: task queue subscription dropped, resubscribing")
		}
	}()

	return out
}

// subscribe opens a dedicated AMQP channel and starts consuming from the
// request queue on it. The channel is dedicated on purpose: a broker-side
// exception on the shared publish channel (or the shared channel being
// reopened) must not silently kill task consumption, and vice versa.
func (c *Consumer) subscribe() (*amqp.Channel, <-chan amqp.Delivery, error) {
	ch, err := c.conn.NewChannel()
	if err != nil {
		return nil, nil, err
	}

	// One task at a time.
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		return nil, nil, err
	}

	// Declare the queue (idempotent) so consuming never fails just because the
	// backend has not provisioned it yet.
	if _, err := ch.QueueDeclare(c.queueName, true, false, false, false, nil); err != nil {
		ch.Close()
		return nil, nil, err
	}

	deliveries, err := ch.Consume(c.queueName, "", false, false, false, false, nil)
	if err != nil {
		ch.Close()
		return nil, nil, err
	}

	c.logger.Info("started consuming from queue", "queue", c.queueName)
	return ch, deliveries, nil
}

// pump forwards deliveries to out until the source channel is closed (the
// subscription died) or ctx is cancelled.
func (c *Consumer) pump(ctx context.Context, deliveries <-chan amqp.Delivery, out chan<- amqp.Delivery) {
	for {
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-deliveries:
			if !ok {
				return
			}
			select {
			case out <- delivery:
			case <-ctx.Done():
				return
			}
		}
	}
}
