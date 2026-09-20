package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrNotConnected = errors.New("not connected to RabbitMQ")

// Publisher handles publishing messages to the response queue
type Publisher struct {
	conn      *Connection
	queueName string
	logger    *slog.Logger
}

// NewPublisher creates a new message publisher
func NewPublisher(conn *Connection, queueName string, logger *slog.Logger) *Publisher {
	return &Publisher{
		conn:      conn,
		queueName: queueName,
		logger:    logger,
	}
}

// DeclareQueue declares the response queue
func (p *Publisher) DeclareQueue() error {
	ch := p.conn.Channel()
	if ch == nil {
		return ErrNotConnected
	}

	_, err := ch.QueueDeclare(
		p.queueName,
		true,  // durable
		false, // autoDelete
		false, // exclusive
		false, // noWait
		nil,
	)
	return err
}

// DeclarePlainQueue declares a durable queue with no arguments. Used for the
// short time-series metrics queues (<hostid>.host_metrics / .vm_metrics), which
// - unlike the last-value inventory queues - must NOT carry x-max-length, so the
// declaration matches ovc-backend's (mismatched args = PRECONDITION_FAILED).
func (p *Publisher) DeclarePlainQueue(queueName string) error {
	ch := p.conn.Channel()
	if ch == nil {
		return ErrNotConnected
	}

	_, err := ch.QueueDeclare(
		queueName,
		true,  // durable
		false, // autoDelete
		false, // exclusive
		false, // noWait
		nil,
	)
	return err
}

// DeclareStateQueue declares a state queue with max-length 1 (keeps only latest message)
func (p *Publisher) DeclareStateQueue(queueName string) error {
	ch := p.conn.Channel()
	if ch == nil {
		return ErrNotConnected
	}

	_, err := ch.QueueDeclare(
		queueName,
		true,  // durable
		false, // autoDelete
		false, // exclusive
		false, // noWait
		amqp.Table{
			"x-max-length": int32(1),
			"x-overflow":   "drop-head", // discard oldest message
		},
	)
	return err
}

// Publish sends a message to the response queue
func (p *Publisher) Publish(ctx context.Context, msg interface{}) error {
	return p.PublishTo(ctx, p.queueName, msg)
}

// PublishConfirmed publishes msg to queueName and blocks until RabbitMQ has
// acknowledged it (publisher confirms) or ctx/timeout expires. It uses a
// dedicated confirm-mode channel and publishes as `mandatory`, so:
//
//   - a broker ack  => the message is safely in the queue; returns nil
//   - a broker nack => returns an error (queue full with reject-publish, etc.)
//   - unroutable    => the mandatory return is caught and returned as an error
//     (wrong queue name, queue vanished)
//   - no ack in time => returns an error (slow/broken link, connection dropped
//     mid-publish)
//
// Unlike Publish, this never reports success for a message the broker did not
// actually receive - important for the short-lived worker process, which exits
// milliseconds after publishing and would otherwise lose an un-flushed message.
func (p *Publisher) PublishConfirmed(ctx context.Context, queueName string, msg interface{}) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	ch, err := p.conn.NewChannel()
	if err != nil {
		return fmt.Errorf("open confirm channel: %w", err)
	}
	defer ch.Close()

	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}

	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))

	pubCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	dc, err := ch.PublishWithDeferredConfirmWithContext(pubCtx, "", queueName, true /* mandatory */, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
	})
	if err != nil {
		return fmt.Errorf("publish to %q: %w", queueName, err)
	}

	acked, err := dc.WaitContext(pubCtx)
	if err != nil {
		return fmt.Errorf("no broker confirm for %q (%d bytes): %w", queueName, len(body), err)
	}
	if !acked {
		return fmt.Errorf("broker NACKed message for %q (%d bytes)", queueName, len(body))
	}

	// A mandatory message the broker could not route comes back on the return
	// channel just before the (still positive) confirm - give it a moment to land.
	select {
	case r := <-returns:
		return fmt.Errorf("message returned as unroutable for %q: %d %s", queueName, r.ReplyCode, r.ReplyText)
	case e := <-closed:
		if e != nil {
			return fmt.Errorf("confirm channel closed while publishing to %q: %d %s", queueName, e.Code, e.Reason)
		}
	case <-time.After(200 * time.Millisecond):
	}

	return nil
}

// PublishTo sends a message to a specific queue
func (p *Publisher) PublishTo(ctx context.Context, queueName string, msg interface{}) error {
	ch := p.conn.Channel()
	if ch == nil {
		return ErrNotConnected
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	publishCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err = ch.PublishWithContext(publishCtx,
		"",        // exchange (default)
		queueName, // routing key
		false,     // mandatory
		false,     // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         body,
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
		},
	)
	if err != nil {
		p.logger.Error("failed to publish message", "queue", queueName, "error", err)
		return err
	}

	return nil
}
