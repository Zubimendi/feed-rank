// Package queue wraps Asynq for FeedRank's async fan-out jobs.
//
// Why Asynq instead of QueueLine?
//   Asynq is a battle-tested, Redis-backed job queue for Go. Since FeedRank
//   already depends on Redis for timeline storage, Asynq shares that
//   infrastructure with zero additional services. The fan-out semantics are
//   identical to what QueueLine would provide: enqueue a job, have a separate
//   worker process consume and execute it.
//
// Job type: "feed:fanout"
// Payload:  { "post_id": int64, "author_id": int64, "score": float64 }
//
// The score (UNIX milliseconds) is included in the payload so the worker can
// ZADD the correct timestamp without a Postgres re-read of the post row.
package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

const TypeFanout = "feed:fanout"

// FanoutPayload is the job payload for a fan-out task.
type FanoutPayload struct {
	PostID   int64   `json:"post_id"`
	AuthorID int64   `json:"author_id"`
	Score    float64 `json:"score"` // UNIX milliseconds — used as Redis sorted-set score
}

// ─────────────────────────────────────────────────────────────────────────────
// Producer (API side)
// ─────────────────────────────────────────────────────────────────────────────

// Client wraps an Asynq client for enqueueing fan-out jobs.
type Client struct {
	asynq *asynq.Client
}

// NewClient creates an Asynq client connected to the given Redis address.
func NewClient(redisAddr string) *Client {
	return &Client{
		asynq: asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr}),
	}
}

// EnqueueFanout enqueues a fan-out job. Returns immediately — the HTTP
// handler can respond to the user before fan-out begins.
//
// asynq.MaxRetry(5): transient Redis hiccups shouldn't drop a job.
// asynq.Queue("fanout"): dedicated queue so fan-out doesn't starve
//   other job types that might be added later.
func (c *Client) EnqueueFanout(ctx context.Context, p FanoutPayload) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("queue.EnqueueFanout marshal: %w", err)
	}

	task := asynq.NewTask(TypeFanout, payload,
		asynq.MaxRetry(5),
		asynq.Queue("fanout"),
	)

	info, err := c.asynq.EnqueueContext(ctx, task)
	if err != nil {
		return fmt.Errorf("queue.EnqueueFanout: %w", err)
	}

	_ = info // available for debug logging if needed
	return nil
}

// Close releases the underlying Asynq client connection.
func (c *Client) Close() error {
	return c.asynq.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Consumer (worker side) — see internal/feed/fanout.go for the handler
// ─────────────────────────────────────────────────────────────────────────────

// ParseFanoutPayload decodes the raw task bytes into a FanoutPayload.
func ParseFanoutPayload(data []byte) (*FanoutPayload, error) {
	var p FanoutPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("queue.ParseFanoutPayload: %w", err)
	}
	return &p, nil
}
