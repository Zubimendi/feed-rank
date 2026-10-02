// cmd/api/main.go — FeedRank HTTP API server.
//
// Startup sequence:
//   1. Load config from env.
//   2. Connect to Postgres (pgxpool) and Redis.
//   3. Wire dependencies: queue client, celebrity reclassifier, feed reader.
//   4. Register chi routes.
//   5. Listen on configured port.
//
// All handlers follow the same pattern:
//   - Decode request body or query params
//   - Call the appropriate internal package function
//   - Write JSON response
//   - Return HTTP errors with structured JSON (not plain text)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/francisco4/feed-rank/internal/cache"
	"github.com/francisco4/feed-rank/internal/celebrity"
	"github.com/francisco4/feed-rank/internal/config"
	"github.com/francisco4/feed-rank/internal/db"
	"github.com/francisco4/feed-rank/internal/feed"
	"github.com/francisco4/feed-rank/internal/queue"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
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

	// ── Dependencies ──────────────────────────────────────────────────────────
	queueClient := queue.NewClient(cfg.RedisURL)
	defer queueClient.Close()

	reclassifier := celebrity.New(pool, cfg.CelebrityThreshold, log)
	feedReader := feed.NewReader(pool, rdb)

	// ── Router ────────────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	s := &server{
		pool:         pool,
		rdb:          rdb,
		queue:        queueClient,
		reclassifier: reclassifier,
		feedReader:   feedReader,
		cfg:          cfg,
		log:          log,
	}

	r.Get("/health", s.handleHealth)
	r.Post("/users", s.handleCreateUser)
	r.Get("/users/{id}", s.handleGetUser)
	r.Post("/follows", s.handleCreateFollow)
	r.Post("/posts", s.handleCreatePost)
	r.Get("/feed", s.handleGetFeed)

	addr := ":" + cfg.Port
	log.Info("api server starting", "addr", addr, "fanout_mode", cfg.FanoutMode)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}

// server holds all shared dependencies for HTTP handlers.
type server struct {
	pool         *pgxpool.Pool
	rdb          *redis.Client
	queue        *queue.Client
	reclassifier *celebrity.Reclassifier
	feedReader   *feed.Reader
	cfg          *config.Config
	log          *slog.Logger
}

// ─────────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────────

// handleHealth returns 200 OK when all dependencies are reachable.
// Used by Docker healthcheck and load balancers.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "postgres unhealthy")
		return
	}
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis unhealthy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "fanout_mode": s.cfg.FanoutMode})
}

// handleCreateUser — POST /users
// Body: { "username": "alice" }
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}

	u, err := db.CreateUser(r.Context(), s.pool, req.Username)
	if err != nil {
		s.log.Error("create user", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// handleGetUser — GET /users/{id}
func (s *server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	u, err := db.GetUser(r.Context(), s.pool, id)
	if err != nil {
		s.log.Error("get user", "error", err)
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleCreateFollow — POST /follows
// Body: { "follower_id": 1, "followee_id": 2 }
//
// After inserting the follow edge, we check whether the followee's new
// follower_count crosses the celebrity threshold and update is_celebrity.
func (s *server) handleCreateFollow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FollowerID int64 `json:"follower_id"`
		FolloweeID int64 `json:"followee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FollowerID == 0 || req.FolloweeID == 0 {
		writeError(w, http.StatusBadRequest, "follower_id and followee_id are required")
		return
	}

	ctx := r.Context()

	newCount, err := db.CreateFollow(ctx, s.pool, req.FollowerID, req.FolloweeID)
	if err != nil {
		s.log.Error("create follow", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create follow")
		return
	}

	// Load the followee to pass the current is_celebrity state to the reclassifier.
	followee, err := db.GetUser(ctx, s.pool, req.FolloweeID)
	if err != nil {
		s.log.Error("get followee for reclassify", "error", err)
		// Non-fatal: the follow was already created.
	} else {
		if err := s.reclassifier.CheckAndReclassify(ctx, followee, newCount); err != nil {
			s.log.Error("reclassify", "error", err)
			// Non-fatal: the follow was already created.
		}
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"follower_id":    req.FollowerID,
		"followee_id":    req.FolloweeID,
		"follower_count": newCount,
	})
}

// handleCreatePost — POST /posts
// Body: { "author_id": 1, "content": "Hello world" }
//
// This handler:
//   1. Writes the post to Postgres.
//   2. Enqueues a fan-out job to Asynq (returns instantly).
//   3. Returns 201 Created — before fan-out begins.
//
// The HTTP response is completely decoupled from fan-out latency.
// The post-creation p99 latency in the load test measures only steps 1+2.
func (s *server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthorID int64  `json:"author_id"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AuthorID == 0 || req.Content == "" {
		writeError(w, http.StatusBadRequest, "author_id and content are required")
		return
	}

	ctx := r.Context()

	// 1. Write post to Postgres — durable record.
	post, err := db.CreatePost(ctx, s.pool, req.AuthorID, req.Content)
	if err != nil {
		s.log.Error("create post", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create post")
		return
	}

	// 2. Enqueue fan-out job. Score = UNIX ms of post timestamp.
	//    The worker will use this score as the Redis sorted-set score,
	//    preserving chronological order without a second DB read.
	score := float64(post.CreatedAt.UnixMilli())
	if err := s.queue.EnqueueFanout(ctx, queue.FanoutPayload{
		PostID:   post.ID,
		AuthorID: post.AuthorID,
		Score:    score,
	}); err != nil {
		// Log but don't fail: the post is already in Postgres.
		// A dead-letter / retry mechanism handles missed fan-outs.
		s.log.Error("enqueue fanout", "post_id", post.ID, "error", err)
	}

	// 3. Respond immediately — fan-out is async.
	writeJSON(w, http.StatusCreated, post)
}

// handleGetFeed — GET /feed?user_id=1&cursor=0&limit=20
//
// cursor=0 means "start from the top" (first page).
// cursor=<score> continues from the last seen score (exclusive).
func (s *server) handleGetFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	userID, err := strconv.ParseInt(q.Get("user_id"), 10, 64)
	if err != nil || userID == 0 {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}

	cursor, _ := strconv.ParseFloat(q.Get("cursor"), 64) // 0 = first page

	limit := 20
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	// Translate cursor=0 to math.MaxFloat64 inside ReadFeed.
	if cursor == 0 {
		cursor = math.MaxFloat64
	}

	page, err := s.feedReader.ReadFeed(r.Context(), userID, cursor, limit)
	if err != nil {
		s.log.Error("read feed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read feed")
		return
	}

	// Also write a helper header so clients can use it directly.
	if page.NextCursor > 0 {
		w.Header().Set("X-Next-Cursor", fmt.Sprintf("%v", page.NextCursor))
	}

	writeJSON(w, http.StatusOK, page)
}

// ─────────────────────────────────────────────────────────────────────────────
// Response helpers
// ─────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Ensure cache import is used (imported for AddToTimeline indirectly via feed/fanout)
var _ = cache.ScoredPost{}
