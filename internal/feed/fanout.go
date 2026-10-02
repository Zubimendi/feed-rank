// Package feed contains the core domain logic for FeedRank.
//
// fanout.go — the fan-out worker handler.
//
// This file is consumed by cmd/worker. It implements the Asynq task handler
// for TypeFanout ("feed:fanout") jobs.
//
// Fan-out routing logic (the core of the hybrid architecture):
//
//   1. Load the post's author from Postgres.
//   2. If the author is NOT a celebrity (is_celebrity=false):
//        → Fan out the post to every follower's timeline sorted set.
//          Each follower gets: ZADD timeline:{followerId} <score> <postId>
//          followed by ZREMRANGEBYRANK to trim the set to TIMELINE_CAP.
//   3. If the author IS a celebrity (is_celebrity=true):
//        → Write only to celebrity_posts:{authorId}.
//          No per-follower ZADDs at all — cost is O(1) regardless of follower count.
//
// This is the key property the load test measures: in "pure" mode, step 3
// doesn't exist and celebrities fan out to all followers — write latency
// scales linearly with follower count. In "hybrid" mode, step 3 is a single
// ZADD — write latency is flat.
package feed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/francisco4/feed-rank/internal/cache"
	"github.com/francisco4/feed-rank/internal/db"
	"github.com/francisco4/feed-rank/internal/queue"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// FanoutHandler implements asynq.Handler for "feed:fanout" tasks.
type FanoutHandler struct {
	pool        *pgxpool.Pool
	rdb         *redis.Client
	timelineCap int
	fanoutMode  string // "hybrid" or "pure"
	log         *slog.Logger
}

// NewFanoutHandler creates a FanoutHandler.
func NewFanoutHandler(pool *pgxpool.Pool, rdb *redis.Client, timelineCap int, fanoutMode string, log *slog.Logger) *FanoutHandler {
	return &FanoutHandler{
		pool:        pool,
		rdb:         rdb,
		timelineCap: timelineCap,
		fanoutMode:  fanoutMode,
		log:         log,
	}
}

// ProcessTask is called by the Asynq worker for every "feed:fanout" job.
// It returns an error to signal job failure (Asynq will retry up to MaxRetry).
func (h *FanoutHandler) ProcessTask(ctx context.Context, t *asynq.Task) error {
	p, err := queue.ParseFanoutPayload(t.Payload())
	if err != nil {
		// Malformed payload — don't retry, it will never succeed.
		return fmt.Errorf("%w: %w", asynq.SkipRetry, err)
	}

	// Load the author to check celebrity status.
	author, err := db.GetUser(ctx, h.pool, p.AuthorID)
	if err != nil {
		return fmt.Errorf("fanout: get author %d: %w", p.AuthorID, err)
	}

	// ─── Routing decision ────────────────────────────────────────────────────
	// In "pure" mode, we ignore is_celebrity and always fan out to all
	// followers. This is the control condition for the load test — it
	// demonstrates the write-latency problem that the hybrid solves.
	if h.fanoutMode == "pure" || !author.IsCelebrity {
		return h.fanoutToFollowers(ctx, p)
	}

	// Celebrity in hybrid mode — write only to their own post list.
	return h.writeCelebrityPost(ctx, p)
}

// fanoutToFollowers pushes postId onto every follower's timeline sorted set.
// For normal accounts, follower counts are bounded so this loop is cheap.
// In pure mode, this is also called for celebrities — expect it to be slow
// at high follower counts (that's the point the load test demonstrates).
func (h *FanoutHandler) fanoutToFollowers(ctx context.Context, p *queue.FanoutPayload) error {
	followerIDs, err := db.GetFollowerIDs(ctx, h.pool, p.AuthorID)
	if err != nil {
		return fmt.Errorf("fanout: get followers: %w", err)
	}

	h.log.Debug("fanning out post to followers",
		"post_id", p.PostID,
		"author_id", p.AuthorID,
		"follower_count", len(followerIDs),
		"mode", h.fanoutMode,
	)

	// ZADD each follower's timeline.
	// TODO for a future version: batch these with a Redis pipeline rather than
	// individual round-trips. For the v1 load test, individual calls are fine
	// and make the latency scaling more obvious in the results.
	for _, fid := range followerIDs {
		if err := cache.AddToTimeline(ctx, h.rdb, fid, p.PostID, p.Score, h.timelineCap); err != nil {
			// Log but don't fail the whole job for one follower.
			h.log.Error("failed to add to timeline",
				"follower_id", fid,
				"post_id", p.PostID,
				"error", err,
			)
		}
	}
	return nil
}

// writeCelebrityPost stores the post in the celebrity's own sorted set.
// O(1) regardless of follower count — no per-follower work at all.
func (h *FanoutHandler) writeCelebrityPost(ctx context.Context, p *queue.FanoutPayload) error {
	h.log.Debug("celebrity post — skipping fan-out",
		"post_id", p.PostID,
		"author_id", p.AuthorID,
	)
	if err := cache.AddCelebrityPost(ctx, h.rdb, p.AuthorID, p.PostID, p.Score); err != nil {
		return fmt.Errorf("fanout: add celebrity post: %w", err)
	}
	return nil
}
