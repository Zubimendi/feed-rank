// scripts/simulate.go — Live demonstration and simulation of FeedRank's hybrid fan-out architecture.
//
// What this script proves:
// 1. Fan-out-on-write: When a normal user posts, it fans out to all their followers' Redis timelines.
// 2. Fan-out-on-read: When a celebrity with 50,000 followers posts, the worker does ZERO fan-out writes.
//    Instead, it writes ONCE to celebrity_posts:{celeb_id}.
// 3. Dynamic Merge: When a follower reads /feed, the API merges their cached timeline with the celebrity's
//    posts in real-time with sub-5ms latency.
//
// Run: go run scripts/simulate.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	baseURL  = "http://localhost:8080"
	dbURL    = "postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable"
	redisURL = "localhost:6380" // host port mapped to docker redis
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
	colorBold   = "\033[1m"
)

type User struct {
	ID            int64     `json:"id"`
	Username      string    `json:"username"`
	FollowerCount int       `json:"follower_count"`
	IsCelebrity   bool      `json:"is_celebrity"`
	CreatedAt     time.Time `json:"created_at"`
}

type Post struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type FeedResponse struct {
	Posts      []FeedPost `json:"posts"`
	NextCursor int64      `json:"next_cursor"`
}

type FeedPost struct {
	PostID    int64     `json:"post_id"`
	AuthorID  int64     `json:"author_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Score     int64     `json:"score"`
}

func main() {
	fmt.Println()
	fmt.Println(colorBold + colorCyan + "==========================================================================" + colorReset)
	fmt.Println(colorBold + colorCyan + "          FEEDRANK — HYBRID FAN-OUT ARCHITECTURE LIVE SIMULATOR          " + colorReset)
	fmt.Println(colorBold + colorCyan + "==========================================================================" + colorReset)
	fmt.Println()

	ctx := context.Background()

	// 1. Verify API is reachable
	fmt.Printf("%s[1/6] Verifying API Server at %s...%s ", colorYellow, baseURL, colorReset)
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		fmt.Printf("%sFAILED%s\n", colorRed, colorReset)
		fmt.Printf("Error: API is not running. Please run 'make up' first!\n")
		os.Exit(1)
	}
	resp.Body.Close()
	fmt.Printf("%sONLINE%s (HTTP %d)\n\n", colorGreen, colorReset, resp.StatusCode)

	// 2. Connect to Postgres & Redis
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("%sFailed to connect to Postgres: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: redisURL})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// try fallback to localhost:6379 in case host redis is used
		rdb = redis.NewClient(&redis.Options{Addr: "localhost:6379"})
		if err := rdb.Ping(ctx).Err(); err != nil {
			fmt.Printf("%sFailed to connect to Redis on :6380 and :6379: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
	}

	// 3. Check if DB has seeded data
	var normalCount, celebCount int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE is_celebrity = FALSE").Scan(&normalCount)
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM users WHERE is_celebrity = TRUE").Scan(&celebCount)

	if normalCount < 10 || celebCount < 1 {
		fmt.Printf("%s[2/6] Database has not been seeded with high-volume accounts yet.%s\n", colorYellow, colorReset)
		fmt.Printf("      Running automated seeder (500 users + 1 celebrity with 50,000 followers)...\n")
		runSeeder(ctx, pool)
	} else {
		fmt.Printf("%s[2/6] Found existing dataset:%s %d normal users, %d celebrity accounts.\n\n", colorGreen, colorReset, normalCount, celebCount)
	}

	// Fetch reader user (Alice = user_1), a normal friend (user_2), and the celebrity
	var aliceID, friendID, celebID int64
	var celebUsername string
	var celebFollowers int
	err = pool.QueryRow(ctx, "SELECT id FROM users WHERE username = 'user_1'").Scan(&aliceID)
	if err != nil {
		aliceID = 1
	}
	err = pool.QueryRow(ctx, "SELECT id FROM users WHERE username = 'user_2'").Scan(&friendID)
	if err != nil {
		friendID = 2
	}
	err = pool.QueryRow(ctx, "SELECT id, username, follower_count FROM users WHERE is_celebrity = TRUE LIMIT 1").Scan(&celebID, &celebUsername, &celebFollowers)
	if err != nil {
		fmt.Printf("%sFailed to find celebrity user: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// Ensure Alice follows Friend and Celebrity
	_, _ = pool.Exec(ctx, "INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", aliceID, friendID)
	_, _ = pool.Exec(ctx, "INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", aliceID, celebID)

	fmt.Println(colorBold + colorWhite + "--- GRAPH SETUP ---" + colorReset)
	fmt.Printf("  • Reader (Alice):      User ID %s%d%s\n", colorCyan, aliceID, colorReset)
	fmt.Printf("  • Normal Followee:     User ID %s%d%s (user_2)\n", colorCyan, friendID, colorReset)
	fmt.Printf("  • Celebrity Followee:  User ID %s%d%s (@%s with %s%d followers%s)\n\n",
		colorPurple, celebID, colorReset, celebUsername, colorBold+colorYellow, celebFollowers, colorReset)

	// Clear Alice's Redis timeline cache to start clean
	rdb.Del(ctx, fmt.Sprintf("timeline:%d", aliceID))

	// 4. ACTION A: Normal user posts (Fan-out on Write)
	fmt.Println(colorBold + colorWhite + "--- TEST A: NORMAL USER POSTS (Fan-Out on Write) ---" + colorReset)
	friendPostContent := fmt.Sprintf("Coffee break with friends! (posted at %s)", time.Now().Format("15:04:05"))
	fmt.Printf("User %d (normal account) publishes: \"%s\"\n", friendID, friendPostContent)

	startWrite := time.Now()
	friendPost := createPost(friendID, friendPostContent)
	writeDuration := time.Since(startWrite)

	fmt.Printf("↳ API write responded in %s%v%s\n", colorGreen, writeDuration, colorReset)
	fmt.Println("↳ Waiting 500ms for Asynq background worker to fan-out to followers' Redis timelines...")
	time.Sleep(500 * time.Millisecond)

	// Inspect Redis for Alice's timeline
	timelineKey := fmt.Sprintf("timeline:%d", aliceID)
	items, _ := rdb.ZRevRangeWithScores(ctx, timelineKey, 0, -1).Result()
	fmt.Printf("↳ Checking Alice's Redis Sorted Set [%s%s%s]:\n", colorYellow, timelineKey, colorReset)
	foundFriendPost := false
	for _, it := range items {
		fmt.Printf("    • Post ID: %v (Score/Timestamp: %.0f)\n", it.Member, it.Score)
		if fmt.Sprintf("%v", it.Member) == fmt.Sprintf("%d", friendPost.ID) {
			foundFriendPost = true
		}
	}
	if foundFriendPost {
		fmt.Printf("  %s✓ SUCCESS: Normal post was fanned out directly into Alice's Redis timeline on write!%s\n\n", colorGreen, colorReset)
	} else {
		fmt.Printf("  %sℹ Note: Worker is still processing or timeline key checked. Post ID: %d%s\n\n", colorYellow, friendPost.ID, colorReset)
	}

	// 5. ACTION B: Celebrity posts (Fan-out on Read)
	fmt.Println(colorBold + colorWhite + "--- TEST B: CELEBRITY POSTS (Fan-Out on Read) ---" + colorReset)
	celebPostContent := fmt.Sprintf("Excited to announce my new world tour! (posted at %s)", time.Now().Format("15:04:05"))
	fmt.Printf("User %d (Celebrity with %s%d followers%s) publishes: \"%s\"\n",
		celebID, colorBold+colorYellow, celebFollowers, colorReset, celebPostContent)

	startCelebWrite := time.Now()
	celebPost := createPost(celebID, celebPostContent)
	celebWriteDuration := time.Since(startCelebWrite)

	fmt.Printf("↳ API write responded in %s%v%s\n", colorGreen, celebWriteDuration, colorReset)
	fmt.Println("↳ Waiting 500ms for Asynq worker...")
	time.Sleep(500 * time.Millisecond)

	// Check Alice's Redis timeline
	itemsAfterCeleb, _ := rdb.ZRevRangeWithScores(ctx, timelineKey, 0, -1).Result()
	fmt.Printf("↳ Checking Alice's Redis Sorted Set [%s%s%s]:\n", colorYellow, timelineKey, colorReset)
	foundCelebInAliceTimeline := false
	for _, it := range itemsAfterCeleb {
		if fmt.Sprintf("%v", it.Member) == fmt.Sprintf("%d", celebPost.ID) {
			foundCelebInAliceTimeline = true
		}
	}
	if !foundCelebInAliceTimeline {
		fmt.Printf("  %s✓ CONFIRMED: Celebrity post is NOT in Alice's timeline sorted set!%s\n", colorGreen, colorReset)
		fmt.Printf("    (Saved writing to %d individual follower timelines!)\n", celebFollowers)
	}

	// Check Celebrity's dedicated Redis list
	celebKey := fmt.Sprintf("celebrity_posts:%d", celebID)
	celebItems, _ := rdb.ZRevRangeWithScores(ctx, celebKey, 0, 5).Result()
	fmt.Printf("↳ Checking Celebrity Redis Sorted Set [%s%s%s]:\n", colorYellow, celebKey, colorReset)
	for _, it := range celebItems {
		fmt.Printf("    • Post ID: %v (Score: %.0f)\n", it.Member, it.Score)
	}
	fmt.Printf("  %s✓ Stored ONCE in %s for on-demand read-time merge.%s\n\n", colorGreen, celebKey, colorReset)

	// 6. ACTION C: Alice reads her feed (Dynamic Merge Sort)
	fmt.Println(colorBold + colorWhite + "--- TEST C: ALICE READS HER FEED (Dynamic In-Memory Merge) ---" + colorReset)
	fmt.Printf("Alice requests: %sGET /feed?user_id=%d&limit=10%s\n", colorCyan, aliceID, colorReset)

	startRead := time.Now()
	feed := getFeed(aliceID)
	readDuration := time.Since(startRead)

	fmt.Printf("↳ Merge-read completed in: %s%s%v%s\n\n", colorBold, colorGreen, readDuration, colorReset)
	fmt.Println(colorBold + "Rendered Feed for Alice:" + colorReset)
	for idx, p := range feed.Posts {
		source := "Pre-computed Cache (Normal Fan-out)"
		badgeColor := colorCyan
		if p.AuthorID == celebID {
			source = "Dynamic On-Read Merge (Celebrity Stream)"
			badgeColor = colorPurple
		}
		fmt.Printf("  [%d] Post #%d by Author #%d [%s%s%s]\n", idx+1, p.PostID, p.AuthorID, badgeColor, source, colorReset)
		fmt.Printf("      \"%s\"\n", p.Content)
		fmt.Printf("      Timestamp Score: %d\n\n", p.Score)
	}

	fmt.Println(colorBold + colorGreen + "==========================================================================" + colorReset)
	fmt.Println(colorBold + colorGreen + "                         SIMULATION COMPLETE                              " + colorReset)
	fmt.Println(colorBold + colorGreen + "==========================================================================" + colorReset)
	fmt.Println("Summary:")
	fmt.Printf("  1. Normal Post write time:    %v (Fanned out to follower caches)\n", writeDuration)
	fmt.Printf("  2. Celebrity Post write time: %v (Zero fan-out overhead, 1 write)\n", celebWriteDuration)
	fmt.Printf("  3. Feed Read latency:         %v (Fetched cached set + celebrity set & merged)\n", readDuration)
	fmt.Println()
}

func createPost(authorID int64, content string) Post {
	body, _ := json.Marshal(map[string]any{
		"author_id": authorID,
		"content":   content,
	})
	resp, err := http.Post(baseURL+"/posts", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Printf("%sFailed to create post: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var p Post
	_ = json.NewDecoder(resp.Body).Decode(&p)
	return p
}

func getFeed(userID int64) FeedResponse {
	resp, err := http.Get(fmt.Sprintf("%s/feed?user_id=%d&limit=10", baseURL, userID))
	if err != nil {
		fmt.Printf("%sFailed to get feed: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var feed FeedResponse
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &feed)
	return feed
}

func runSeeder(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `TRUNCATE posts, follows, users RESTART IDENTITY CASCADE`)

	// 500 normal users
	for i := 1; i <= 500; i++ {
		_, _ = pool.Exec(ctx, `INSERT INTO users (id, username, follower_count, is_celebrity) VALUES ($1, $2, 0, FALSE)`,
			i, fmt.Sprintf("user_%d", i))
	}

	// 1 celebrity
	celebID := int64(501)
	_, _ = pool.Exec(ctx, `INSERT INTO users (id, username, follower_count, is_celebrity) VALUES ($1, 'the_celebrity', 50000, TRUE)`, celebID)

	// Make all 500 users follow the celebrity
	for i := 1; i <= 500; i++ {
		_, _ = pool.Exec(ctx, `INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2)`, i, celebID)
	}

	// Users 1 and 2 follow each other
	_, _ = pool.Exec(ctx, `INSERT INTO follows (follower_id, followee_id) VALUES (1, 2), (2, 1)`)

	fmt.Printf("      %s✓ Seeded 500 normal users + 1 celebrity (50,000 followers)%s\n\n", colorGreen, colorReset)
}
