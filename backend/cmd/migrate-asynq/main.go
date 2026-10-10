// migrate-asynq inventories a stopped legacy Redis and stages its pending work
// in PostgreSQL. Default is validation only; --apply commits the transfer.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/outbox"
	"github.com/11bronx11/bx_yunpan/backend/internal/taskqueue"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	apply := flag.Bool("apply", false, "stage validated legacy work in PostgreSQL; never modifies Redis")
	gcDelay := flag.Duration("gc-delay", 24*time.Hour, "minimum original GC delay")
	flag.Parse()
	addr, dsn := os.Getenv("LEGACY_REDIS_ADDR"), os.Getenv("POSTGRES_DSN")
	if addr == "" || dsn == "" {
		return fmt.Errorf("LEGACY_REDIS_ADDR and POSTGRES_DSN required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("LEGACY_REDIS_PASSWORD")})
	defer client.Close()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("open migration database failed")
	}
	sql, _ := db.DB()
	defer sql.Close()
	type item struct {
		task taskqueue.Task
		due  time.Time
	}
	var tasks []item
	counts := map[string]int{}
	iter := client.Scan(ctx, 0, "asynq:*:t:*", 1000).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		fields, err := client.HMGet(ctx, key, "state", "msg").Result()
		if err != nil {
			return err
		}
		state, _ := fields[0].(string)
		counts[state]++
		if state == "archived" || state == "completed" {
			continue
		}
		if state != "pending" && state != "retry" && state != "scheduled" {
			return fmt.Errorf("legacy state %q requires stopped-worker reconciliation", state)
		}
		raw, ok := fields[1].(string)
		if !ok {
			return fmt.Errorf("legacy task message missing")
		}
		task, err := taskqueue.DecodeLegacyTask([]byte(raw))
		if err != nil {
			return err
		}
		due := time.Now().UTC()
		if state == "retry" || state == "scheduled" {
			prefix := key[:strings.LastIndex(key, ":t:")]
			score, err := client.ZScore(ctx, prefix+":"+state, task.ID).Result()
			if err != nil {
				return fmt.Errorf("legacy schedule missing: %w", err)
			}
			scheduled := time.Unix(int64(score), 0)
			if scheduled.After(due) {
				due = scheduled
			}
		}
		tasks = append(tasks, item{task, due})
	}
	if err := iter.Err(); err != nil {
		return err
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, item := range tasks {
			if err := outbox.StageLegacyTask(tx, item.task, item.due, *gcDelay, *apply); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("legacy_states=%v outstanding=%d applied=%t source_redis_unchanged=true\n", counts, len(tasks), *apply)
	return nil
}
