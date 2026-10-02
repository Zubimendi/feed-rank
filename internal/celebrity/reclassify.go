// Package celebrity handles threshold-based reclassification of accounts.
//
// Design decision (from ARCHITECTURE.md):
//   When an account's follower_count crosses CELEBRITY_THRESHOLD, the worker's
//   fan-out strategy must change for future posts. We store this as a boolean
//   flag (is_celebrity) on the users row rather than re-reading follower_count
//   on every fan-out job — one indexed boolean read is cheaper than one COUNT
//   join, and the flag is set here atomically with the follower_count update.
//
// No backfill:
//   When an account flips to celebrity, existing entries in followers' timeline
//   sorted sets simply age out via the normal trim (ZREMRANGEBYRANK cap).
//   There is no retroactive cleanup. This is an explicit, low-cost design choice
//   documented in ARCHITECTURE.md §"What's out of scope".
package celebrity

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/francisco4/feed-rank/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reclassifier checks whether a user should be promoted to (or demoted from)
// celebrity status and updates the database accordingly.
type Reclassifier struct {
	pool      *pgxpool.Pool
	threshold int
	log       *slog.Logger
}

// New returns a Reclassifier with the given threshold.
func New(pool *pgxpool.Pool, threshold int, log *slog.Logger) *Reclassifier {
	return &Reclassifier{pool: pool, threshold: threshold, log: log}
}

// CheckAndReclassify is called after every CreateFollow.
//
// It receives the user whose follower_count just changed (the followee) and
// the new count returned by db.CreateFollow. If the count crosses the
// threshold in either direction, it updates is_celebrity.
//
// Calling this in the HTTP handler (synchronously, after CreateFollow) is
// intentional: reclassification is a single SQL UPDATE, not a fan-out, so it
// doesn't meaningfully affect the follow response latency.
func (r *Reclassifier) CheckAndReclassify(ctx context.Context, user *db.User, newFollowerCount int) error {
	shouldBeCeleb := newFollowerCount >= r.threshold

	if user.IsCelebrity == shouldBeCeleb {
		// No change needed.
		return nil
	}

	if err := db.SetCelebrity(ctx, r.pool, user.ID, shouldBeCeleb); err != nil {
		return fmt.Errorf("celebrity.CheckAndReclassify: %w", err)
	}

	r.log.Info("celebrity status changed",
		"user_id", user.ID,
		"username", user.Username,
		"follower_count", newFollowerCount,
		"is_celebrity", shouldBeCeleb,
		"threshold", r.threshold,
	)
	return nil
}
