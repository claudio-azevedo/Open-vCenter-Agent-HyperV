package jobqueue

import (
	"context"
	"log/slog"
	"time"

	"ovc-agent/internal/jobstore"
)

// Retention periodically purges terminal job records from the store.
// Jobs in completed, failed, or timeout status older than 24 hours are deleted.
type Retention struct {
	store  jobstore.Store
	logger *slog.Logger
}

// NewRetention creates a Retention instance that purges old job records.
func NewRetention(store jobstore.Store, logger *slog.Logger) *Retention {
	return &Retention{store: store, logger: logger}
}

// Run starts the retention ticker, scanning every 60 minutes until ctx is cancelled.
func (r *Retention) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.purge()
		}
	}
}

// purge deletes job records older than 24 hours in terminal states.
func (r *Retention) purge() {
	deleted, err := r.store.DeleteOlderThan(24*time.Hour, []string{
		jobstore.StatusCompleted,
		jobstore.StatusFailed,
		jobstore.StatusTimeout,
	})
	if err != nil {
		r.logger.Error("retention purge failed", "error", err)
		return
	}
	if deleted > 0 {
		r.logger.Info("retention purge completed", "deleted", deleted)
	}
}
