package rabbitmq

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 60 * time.Second

	// watchdogInterval is how often the supervisor re-checks the health of the
	// connection even when no close notification arrived. It is the safety net
	// that guarantees the agent can never stay disconnected forever because a
	// close notification was missed.
	watchdogInterval = 15 * time.Second
)

// Connection manages the RabbitMQ connection with automatic reconnection.
//
// A single supervisor goroutine owns recovery: it waits for either a broker
// close notification or a watchdog tick that finds the connection dead, and
// then redials with exponential backoff. Everything else (publishers, the
// consumer) just asks for the current channel and gets ErrNotConnected while
// recovery is in progress.
type Connection struct {
	url    string
	logger *slog.Logger

	mu        sync.RWMutex
	conn      *amqp.Connection
	channel   *amqp.Channel
	connClose chan *amqp.Error

	closeChan chan struct{}
	closeOnce sync.Once
	superOnce sync.Once

	// Notify subscribers of reconnection
	onReconnect []func()
}

// NewConnection creates a new RabbitMQ connection manager
func NewConnection(url string, logger *slog.Logger) *Connection {
	return &Connection{
		url:       url,
		logger:    logger,
		closeChan: make(chan struct{}),
	}
}

// Connect establishes the initial connection to RabbitMQ with retry loop.
// It will keep retrying with exponential backoff until connected or context is
// cancelled. Once connected, a supervisor goroutine keeps the connection alive
// for the lifetime of the process (it is not tied to the connect context, which
// may be short-lived).
func (c *Connection) Connect(ctx context.Context) error {
	if err := c.dialWithRetry(ctx); err != nil {
		return err
	}

	c.superOnce.Do(func() {
		go c.supervise()
	})
	return nil
}

func (c *Connection) dialWithRetry(ctx context.Context) error {
	attempt := 0
	for {
		err := c.dial()
		if err == nil {
			return nil
		}

		attempt++
		backoff := calculateBackoff(attempt)
		c.logger.Error("failed to connect to RabbitMQ, retrying",
			"attempt", attempt,
			"backoff", backoff.String(),
			"error", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closeChan:
			return err
		case <-time.After(backoff):
			// retry
		}
	}
}

// dial opens a fresh connection plus shared channel and publishes them as the
// current ones, discarding whatever was there before.
func (c *Connection) dial() error {
	var (
		conn *amqp.Connection
		err  error
	)
	if strings.HasPrefix(c.url, "amqps://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
		}
		conn, err = amqp.DialTLS(c.url, tlsConfig)
	} else {
		conn, err = amqp.Dial(c.url)
	}
	if err != nil {
		return err
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}

	// Register the close notification unconditionally, before the connection is
	// published as the current one. amqp091 closes the receiver when the
	// connection is already gone, so the supervisor is woken up either way.
	// Skipping the registration for an already-dead connection (as an earlier
	// version did) left the supervisor blocked forever on a channel nobody
	// would ever signal, and the agent stayed disconnected for good.
	closeCh := conn.NotifyClose(make(chan *amqp.Error, 1))

	c.mu.Lock()
	old := c.conn
	c.conn = conn
	c.channel = ch
	c.connClose = closeCh
	c.mu.Unlock()

	if old != nil && !old.IsClosed() {
		old.Close()
	}

	c.logger.Info("connected to RabbitMQ")
	return nil
}

// supervise is the single owner of reconnection. It runs until Close.
func (c *Connection) supervise() {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		c.mu.RLock()
		closeCh := c.connClose
		c.mu.RUnlock()

		select {
		case <-c.closeChan:
			return

		case amqpErr := <-closeCh:
			if amqpErr != nil {
				c.logger.Error("RabbitMQ connection lost", "error", amqpErr)
			} else {
				c.logger.Warn("RabbitMQ connection closed")
			}

		case <-ticker.C:
			if c.healthy() {
				continue
			}
			c.logger.Warn("RabbitMQ watchdog found the connection down, forcing reconnect")
		}

		if !c.recover() {
			return
		}
	}
}

// healthy reports whether the connection and its shared channel are usable.
// When only the shared channel died (a channel-level exception leaves the
// connection open) it is reopened in place instead of dropping the connection.
func (c *Connection) healthy() bool {
	c.mu.RLock()
	conn := c.conn
	ch := c.channel
	c.mu.RUnlock()

	if conn == nil || conn.IsClosed() {
		return false
	}
	if ch == nil || ch.IsClosed() {
		c.logger.Warn("RabbitMQ shared channel is closed, reopening")
		return c.reopenChannel(conn)
	}
	return true
}

// reopenChannel replaces the shared channel with a fresh one on the existing
// connection. Returns false when that fails, so the caller falls back to a full
// reconnect.
func (c *Connection) reopenChannel(conn *amqp.Connection) bool {
	newCh, err := conn.Channel()
	if err != nil {
		c.logger.Error("failed to reopen shared RabbitMQ channel", "error", err)
		return false
	}

	c.mu.Lock()
	if c.conn != conn {
		// A reconnect landed in the meantime - the new connection already has
		// its own channel, drop this one.
		c.mu.Unlock()
		newCh.Close()
		return true
	}
	c.channel = newCh
	c.mu.Unlock()

	c.logger.Info("reopened shared RabbitMQ channel")
	return true
}

// recover tears down the dead connection and redials until it succeeds.
// Returns false only when the manager is being closed.
func (c *Connection) recover() bool {
	// Drop the dead connection first so publishers fail fast with
	// ErrNotConnected instead of publishing into a half-open socket.
	c.mu.Lock()
	old := c.conn
	c.conn = nil
	c.channel = nil
	c.connClose = nil
	c.mu.Unlock()

	if old != nil {
		old.Close()
	}

	c.logger.Info("attempting to reconnect to RabbitMQ")
	if err := c.dialWithRetry(context.Background()); err != nil {
		return false
	}
	c.logger.Info("successfully reconnected to RabbitMQ")

	c.mu.RLock()
	handlers := make([]func(), len(c.onReconnect))
	copy(handlers, c.onReconnect)
	c.mu.RUnlock()

	// Handlers do broker round-trips (queue declares) that can block; run them
	// off the supervisor goroutine so the watchdog stays responsive no matter
	// what a handler does.
	if len(handlers) > 0 {
		go func() {
			for _, handler := range handlers {
				handler()
			}
		}()
	}
	return true
}

func calculateBackoff(attempt int) time.Duration {
	backoff := float64(initialBackoff) * math.Pow(2, float64(attempt-1))
	if backoff > float64(maxBackoff) {
		backoff = float64(maxBackoff)
	}
	return time.Duration(backoff)
}

// Channel returns the current AMQP channel, or nil when there is no usable one
// (disconnected, or the channel was closed by a broker-side exception).
func (c *Connection) Channel() *amqp.Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.channel == nil || c.channel.IsClosed() {
		return nil
	}
	return c.channel
}

// NewChannel opens a fresh AMQP channel on the current connection. Useful for
// operations that may close the channel on failure (e.g. passive queue declares)
// or that need to be isolated from the shared channel (publisher confirms, the
// task consumer). The caller is responsible for closing it.
func (c *Connection) NewChannel() (*amqp.Channel, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil || conn.IsClosed() {
		return nil, ErrNotConnected
	}
	return conn.Channel()
}

// OnReconnect registers a callback to be called after successful reconnection
func (c *Connection) OnReconnect(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReconnect = append(c.onReconnect, fn)
}

// Close shuts down the connection
func (c *Connection) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeChan)
	})

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil && !c.conn.IsClosed() {
		return c.conn.Close()
	}
	return nil
}

// IsConnected returns true if the connection is active
func (c *Connection) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && !c.conn.IsClosed()
}
