// cmd/worker/main.go — FeedRank fan-out worker.
//
// This binary is the Asynq consumer that processes "feed:fanout" jobs.
// It runs independently from the API server so that:
//   - Fan-out failures don't affect API availability.
//   - The worker can be scaled horizontally (multiple instances process
//     different jobs concurrently from the Asynq queue).
//   - The API server's response latency is never blocked on fan-out work.
//
// Startup:
//   1. Load config from env (same env vars as the API).
//   2. Connect to Postgres and Redis.
//   3. Register the FanoutHandler for TypeFanout tasks.
//   4. Start the Asynq server (blocking).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/francisco4/feed-rank/internal/config"
	"github.com/francisco4/feed-rank/internal/feed"
	"github.com/francisco4/feed-rank/internal/queue"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// ── Postgres ──────────────────────────────────────────────────────────────
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("postgres connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Error("postgres ping failed", "error", err)
		os.Exit(1)
	}
	log.Info("postgres connected")

	// ── Redis ─────────────────────────────────────────────────────────────────
	opt, err := redis.ParseURL("redis://" + cfg.RedisURL)
	if err != nil {
		log.Error("redis parse url failed", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Error("redis ping failed", "error", err)
		os.Exit(1)
	}
	defer rdb.Close()
	log.Info("redis connected")

	// ── Fan-out handler ───────────────────────────────────────────────────────
	// fanoutMode is read from env so both API and worker agree on strategy.
	// In load-test runs, you spin up worker with FANOUT_MODE=pure to measure
	// the control condition.
	handler := feed.NewFanoutHandler(pool, rdb, cfg.TimelineCap, cfg.FanoutMode, log)

	// ── Asynq server ──────────────────────────────────────────────────────────
	// Concurrency: 10 goroutines per worker instance.
	// For the load test, you can scale this up via docker-compose replicas.
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.RedisURL},
		asynq.Config{
			Concurrency: 10,
			Queues: map[string]int{
				"fanout":  10, // dedicated queue — matches producer's Queue("fanout")
				"default": 1,
			},
			Logger: newAsynqLogger(log),
		},
	)

	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TypeFanout, handler.ProcessTask)

	log.Info("worker starting",
		"fanout_mode", cfg.FanoutMode,
		"celebrity_threshold", cfg.CelebrityThreshold,
		"timeline_cap", cfg.TimelineCap,
	)

	if err := srv.Run(mux); err != nil {
		log.Error("worker error", "error", err)
		os.Exit(1)
	}
}

// asynqLogger adapts slog to Asynq's Logger interface.
type asynqLogger struct{ log *slog.Logger }

func newAsynqLogger(l *slog.Logger) *asynqLogger { return &asynqLogger{log: l} }

func (a *asynqLogger) Debug(args ...interface{}) { a.log.Debug("asynq", "msg", args) }
func (a *asynqLogger) Info(args ...interface{})  { a.log.Info("asynq", "msg", args) }
func (a *asynqLogger) Warn(args ...interface{})  { a.log.Warn("asynq", "msg", args) }
func (a *asynqLogger) Error(args ...interface{}) { a.log.Error("asynq", "msg", args) }
func (a *asynqLogger) Fatal(args ...interface{}) {
	a.log.Error("asynq fatal", "msg", args)
	os.Exit(1)
}
