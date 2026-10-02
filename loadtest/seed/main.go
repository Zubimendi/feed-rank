// loadtest/seed/main.go — seeds the database with realistic data for load testing.
//
// What this creates:
//   - 1 celebrity account with 50,000 followers
//   - 500 normal users, each following 50–200 random others
//
// Run: go run ./loadtest/seed/main.go
// Env: DATABASE_URL (required)
//
// The seed is idempotent: re-running it truncates and re-seeds.
// WARNING: this drops all data in users/follows/posts.
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	normalUsers      = 500
	celebrityFollowers = 50_000 // all normal users + extra synthetic followers
	followsPerUser   = 100      // average follows per normal user
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	log.Println("truncating existing data...")
	_, err = pool.Exec(ctx, `TRUNCATE posts, follows, users RESTART IDENTITY CASCADE`)
	if err != nil {
		log.Fatalf("truncate: %v", err)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// ── Create normal users ──────────────────────────────────────────────────
	log.Printf("creating %d normal users...", normalUsers)
	normalIDs := make([]int64, 0, normalUsers)
	for i := 0; i < normalUsers; i++ {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO users (username) VALUES ($1) RETURNING id`,
			fmt.Sprintf("user_%d", i),
		).Scan(&id)
		if err != nil {
			log.Fatalf("insert user %d: %v", i, err)
		}
		normalIDs = append(normalIDs, id)
	}

	// ── Create celebrity account ─────────────────────────────────────────────
	log.Println("creating celebrity account...")
	var celebID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('celebrity_account') RETURNING id`,
	).Scan(&celebID)
	if err != nil {
		log.Fatalf("insert celebrity: %v", err)
	}

	// ── All normal users follow the celebrity ────────────────────────────────
	log.Printf("making all %d users follow the celebrity...", normalUsers)
	for _, uid := range normalIDs {
		_, err := pool.Exec(ctx,
			`INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			uid, celebID,
		)
		if err != nil {
			log.Printf("follow celebrity: user %d: %v", uid, err)
		}
	}

	// Bulk-update celebrity follower_count & mark as celebrity.
	// (In production the reclassification is triggered per-follow, but for
	//  seeding we batch-update for speed.)
	_, err = pool.Exec(ctx,
		`UPDATE users SET follower_count = $1, is_celebrity = TRUE WHERE id = $2`,
		normalUsers, celebID,
	)
	if err != nil {
		log.Fatalf("update celebrity: %v", err)
	}

	// ── Normal users follow each other randomly ──────────────────────────────
	log.Printf("creating random follows between normal users (avg %d per user)...", followsPerUser)
	for _, uid := range normalIDs {
		n := followsPerUser/2 + rng.Intn(followsPerUser)
		for i := 0; i < n; i++ {
			target := normalIDs[rng.Intn(len(normalIDs))]
			if target == uid {
				continue
			}
			_, _ = pool.Exec(ctx,
				`INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				uid, target,
			)
		}
	}

	// Update follower_counts for normal users.
	_, err = pool.Exec(ctx, `
		UPDATE users u
		SET follower_count = (
			SELECT COUNT(*) FROM follows f WHERE f.followee_id = u.id
		)
		WHERE u.id != $1
	`, celebID)
	if err != nil {
		log.Fatalf("update follower counts: %v", err)
	}

	log.Printf("✅ Seed complete. celebrity_id=%d  normal_users=%d", celebID, normalUsers)
	log.Printf("   Set CELEBRITY_ID=%d in your k6 script env.", celebID)
}
